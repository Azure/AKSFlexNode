package v20260301

import (
	"context"
	"fmt"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v8"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/hybridcompute/armhybridcompute"
	"github.com/Azure/go-autorest/autorest/to"

	"github.com/Azure/AKSFlexNode/components/arc"
	"github.com/Azure/AKSFlexNode/pkg/utils"
)

// Arc machine registration and RBAC methods

func (a *installArcAction) registerArcMachine(ctx context.Context, spec *arc.InstallArcSpec) (*armhybridcompute.Machine, error) {
	a.logger.Info("Registering machine with Azure Arc using Arc agent")

	// Check if already registered
	machine, err := a.getArcMachine(ctx, spec)
	if err == nil && machine != nil {
		a.logger.Infof("Machine already registered as Arc machine: %s", to.String(machine.Name))
		return machine, nil
	}

	// Register using Arc agent command
	if err := a.runArcAgentConnect(ctx, spec); err != nil {
		return nil, fmt.Errorf("failed to register Arc machine using agent: %w", err)
	}

	// make sure registration is complete before proceeding
	// otherwise role assignment may fail due to identity not found
	return a.waitForArcRegistration(ctx, spec)
}

func (a *installArcAction) getArcMachine(ctx context.Context, spec *arc.InstallArcSpec) (*armhybridcompute.Machine, error) {
	machineName := spec.GetMachineName()
	resourceGroup := spec.GetResourceGroup()

	a.logger.Infof("Getting Arc machine info for: %s in resource group: %s", machineName, resourceGroup)
	result, err := a.hybridComputeMachineClient.Get(ctx, resourceGroup, machineName, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get Arc machine info via SDK: %w", err)
	}
	machine := result.Machine
	a.logger.Infof("Successfully retrieved Arc machine info: %s (ID: %s)", to.String(machine.Name), to.String(machine.ID))
	return &result.Machine, nil
}

func (a *installArcAction) validateManagedCluster(ctx context.Context, spec *arc.InstallArcSpec) error {
	a.logger.Info("Validating target AKS Managed Cluster requirements for Azure RBAC authentication")

	cluster, err := a.getAKSCluster(ctx, spec)
	if err != nil {
		return fmt.Errorf("failed to get AKS cluster info: %w", err)
	}

	// Check if Azure RBAC is enabled
	if cluster.Properties == nil ||
		cluster.Properties.AADProfile == nil ||
		cluster.Properties.AADProfile.EnableAzureRBAC == nil ||
		!*cluster.Properties.AADProfile.EnableAzureRBAC {
		return fmt.Errorf("target AKS cluster '%s' must have Azure RBAC enabled for node authentication", to.String(cluster.Name))
	}

	a.logger.Infof("Target AKS cluster '%s' has Azure RBAC enabled", to.String(cluster.Name))
	return nil
}

func (a *installArcAction) getAKSCluster(ctx context.Context, spec *arc.InstallArcSpec) (*armcontainerservice.ManagedCluster, error) {
	clusterName := spec.GetAksClusterName()
	// For now, assume cluster is in same resource group as Arc machine
	// In future, this could be specified separately in spec
	clusterResourceGroup := spec.GetResourceGroup()

	a.logger.Infof("Getting AKS cluster info for: %s in resource group: %s", clusterName, clusterResourceGroup)
	result, err := a.mcClient.Get(ctx, clusterResourceGroup, clusterName, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get AKS cluster info via SDK: %w", err)
	}
	cluster := result.ManagedCluster
	a.logger.Infof("Successfully retrieved AKS cluster info: %s", to.String(cluster.Name))
	return &result.ManagedCluster, nil
}

func (a *installArcAction) waitForArcRegistration(ctx context.Context, spec *arc.InstallArcSpec) (*armhybridcompute.Machine, error) {
	const (
		maxRetries   = 10
		initialDelay = 5 * time.Second
		maxDelay     = 30 * time.Second
	)

	for attempt := 0; attempt < maxRetries; attempt++ {
		machine, err := a.getArcMachine(ctx, spec)
		if err == nil &&
			machine != nil &&
			machine.Identity != nil &&
			machine.Identity.PrincipalID != nil {
			return machine, nil // Success!
		}
		a.logger.Infof("Arc machine not yet registered (attempt %d/%d): %s", attempt+1, maxRetries, err)

		delay := min(initialDelay*time.Duration(1<<attempt), maxDelay)
		a.logger.Infof("Registration attempt %d/%d, waiting %v...", attempt+1, maxRetries, delay)

		select {
		case <-time.After(delay):
			continue
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	return nil, fmt.Errorf("arc registration timed out after %d attempts", maxRetries)
}

func (a *installArcAction) runArcAgentConnect(ctx context.Context, spec *arc.InstallArcSpec) error {
	a.logger.Info("Connecting machine to Azure Arc using azcmagent")

	// Build azcmagent connect command
	args := []string{
		"connect",
		"--resource-group", spec.GetResourceGroup(),
		"--tenant-id", spec.GetTenantId(),
		"--location", spec.GetLocation(),
		"--subscription-id", spec.GetSubscriptionId(),
		"--resource-name", spec.GetMachineName(),
	}

	// Add Arc tags if any
	tags := spec.GetTags()
	tagArgs := []string{}
	for key, value := range tags {
		tagArgs = append(tagArgs, "--tags", fmt.Sprintf("%s=%s", key, value))
	}
	args = append(args, tagArgs...)

	// Add authentication parameters
	if err := a.addAuthenticationArgs(ctx, &args); err != nil {
		return fmt.Errorf("failed to configure authentication for Arc agent: %w", err)
	}

	// Execute azcmagent command securely (avoid logging access token)
	if err := a.runAzcmagentSecurely("azcmagent", args); err != nil {
		return fmt.Errorf("failed to connect to Azure Arc: %w", err)
	}

	a.logger.Infof("Arc agent connect completed")
	return nil
}

func (a *installArcAction) addAuthenticationArgs(ctx context.Context, args *[]string) error {
	// Get credential and access token for Arc agent authentication
	cred, err := a.getCredential(ctx)
	if err != nil {
		return fmt.Errorf("failed to obtain credential: %w", err)
	}

	// Get access token for Azure Resource Manager
	accessToken, err := a.authProvider.GetAccessToken(ctx, cred)
	if err != nil {
		return fmt.Errorf("failed to get access token: %w", err)
	}

	// TODO(security): The access token is passed on the command line and is therefore visible
	// to all local users via /proc/<pid>/cmdline for the lifetime of the azcmagent process.
	// The token is short-lived (~60 minutes) which limits the exposure window, but it is
	// still observable during the registration process. azcmagent does not currently support
	// reading the token from stdin or an environment variable.
	// TODO: Check if we can let azcmagent discover auth settings on its own (e.g. --use-azcli
	// or VM MSI) instead of fetching a token ourselves and passing it on the command line.
	*args = append(*args, "--access-token", accessToken)

	a.logger.Debug("Authentication arguments added to Arc agent command")
	return nil
}

func (a *installArcAction) runAzcmagentSecurely(name string, args []string) error {
	// Create command args without exposing the access token in logs
	maskedArgs := make([]string, len(args))
	copy(maskedArgs, args)

	// Find and mask the access token
	for j := 0; j < len(maskedArgs)-1; j++ {
		if maskedArgs[j] == "--access-token" {
			maskedArgs[j+1] = "***REDACTED***"
			break
		}
	}

	a.logger.Infof("Executing command: %s %v", name, maskedArgs)

	// Execute the actual command with real args but capture output to avoid logging
	_, err := utils.RunCommandWithOutput(name, args...)
	if err != nil {
		return err
	}

	return nil
}
