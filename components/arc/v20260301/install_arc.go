package v20260301

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/authorization/armauthorization/v3"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v5"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/hybridcompute/armhybridcompute"
	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/Azure/AKSFlexNode/components/api"
	"github.com/Azure/AKSFlexNode/components/arc"
	"github.com/Azure/AKSFlexNode/components/services/actions"
	"github.com/Azure/AKSFlexNode/pkg/auth"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilpb"
)

type installArcAction struct {
	logger                     *logrus.Logger
	authProvider               *auth.AuthProvider
	hybridComputeMachineClient *armhybridcompute.MachinesClient
	mcClient                   *armcontainerservice.ManagedClustersClient
	roleAssignmentsClient      *armauthorization.RoleAssignmentsClient
}

func newInstallArcAction() (actions.Server, error) {
	return &installArcAction{
		logger:       logrus.New(),
		authProvider: auth.NewAuthProvider(),
	}, nil
}

var _ actions.Server = (*installArcAction)(nil)

func (a *installArcAction) ApplyAction(
	ctx context.Context,
	req *actions.ApplyActionRequest,
) (*actions.ApplyActionResponse, error) {
	config, err := utilpb.AnyTo[*arc.InstallArc](req.GetItem())
	if err != nil {
		return nil, err
	}

	// Apply defaults and validate the configuration
	spec, err := api.DefaultAndValidate(config.GetSpec())
	if err != nil {
		return nil, err
	}

	a.logger.Info("Starting Arc installation...")

	// Execute the arc installation workflow
	var finalPhase string
	var success bool
	var errorMessage string

	// Step 1: Validate prerequisites
	if err := a.ensurePrerequisites(ctx); err != nil {
		finalPhase = "validation"
		success = false
		errorMessage = fmt.Sprintf("validation failed: %v", err)
	} else {
		// Step 2: Execute Arc installation
		if err := a.execute(ctx, spec); err != nil {
			finalPhase = "installation"
			success = false
			errorMessage = fmt.Sprintf("installation failed: %v", err)
		} else {
			// Step 3: Verify completion
			if !a.isCompleted(ctx) {
				finalPhase = "verification"
				success = false
				errorMessage = "installation completed but verification failed"
			} else {
				// Success!
				finalPhase = "completed"
				success = true
				errorMessage = ""
				a.logger.Info("Arc installation completed successfully")
			}
		}
	}

	// Set final status using builder pattern
	st := arc.InstallArcStatus_builder{
		Phase:        to.Ptr(finalPhase),
		Success:      to.Ptr(success),
		ErrorMessage: to.Ptr(errorMessage),
	}

	config.SetStatus(st.Build())

	item, err := anypb.New(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create response: %w", err)
	}

	return actions.ApplyActionResponse_builder{Item: item}.Build(), nil
}

// ensurePrerequisites validates prerequisites and sets up environment for Arc installation
func (a *installArcAction) ensurePrerequisites(ctx context.Context) error {
	a.logger.Info("Validating prerequisites...")

	// Ensure authentication is ready for Arc agent setup
	a.logger.Info("Checking Azure authentication...")
	if err := a.ensureAuthentication(ctx); err != nil {
		a.logger.Errorf("Authentication setup failed: %v", err)
		return fmt.Errorf("arc bootstrap setup failed at authentication: %w", err)
	}
	a.logger.Info("Azure authentication verified successfully")

	// Ensure Arc agent is installed
	a.logger.Info("Checking Arc agent installation...")
	if !isArcAgentInstalled() {
		a.logger.Info("Azure Arc agent not found, installing...")
		if err := a.installArcAgentBinary(ctx); err != nil {
			return fmt.Errorf("failed to install Azure Arc agent binary: %w", err)
		}
	} else {
		a.logger.Info("Azure Arc agent is already installed")
	}

	a.logger.Info("Prerequisites validation completed")
	return nil
}

// execute performs Arc setup
func (a *installArcAction) execute(ctx context.Context, spec *arc.InstallArcSpec) error {
	a.logger.Info("Setting up Arc bootstrap process...")

	// Set up Azure SDK clients
	a.logger.Info("Initializing Azure clients...")
	if err := a.setUpClients(ctx, spec.GetSubscriptionId()); err != nil {
		return fmt.Errorf("arc bootstrap setup failed at client setup: %w", err)
	}

	// Register Arc machine with Azure
	a.logger.Info("Registering Arc machine with Azure...")
	arcMachine, err := a.registerArcMachine(ctx, spec)
	if err != nil {
		a.logger.Errorf("Failed to register Arc machine: %v", err)
		return fmt.Errorf("arc bootstrap setup failed at machine registration: %w", err)
	}
	a.logger.Info("Arc machine registered successfully")

	// Validate managed cluster requirements
	a.logger.Info("Validating managed cluster requirements...")
	if err := a.validateManagedCluster(ctx, spec); err != nil {
		a.logger.Errorf("Managed Cluster validation failed: %v", err)
		return fmt.Errorf("arc bootstrap setup failed at managed cluster validation: %w", err)
	}

	// Assign RBAC roles to managed identity
	a.logger.Info("Waiting for identity to be ready...")
	time.Sleep(10 * time.Second) // brief pause to ensure identity is ready
	a.logger.Info("Assigning RBAC roles to managed identity...")
	if err := a.assignRBACRoles(ctx, arcMachine, spec); err != nil {
		a.logger.Errorf("Failed to assign RBAC roles: %v", err)
		return fmt.Errorf("arc bootstrap setup failed at RBAC role assignment: %w", err)
	}
	a.logger.Info("RBAC roles assigned successfully")

	a.logger.Info("Arc bootstrap setup completed successfully")
	return nil
}

// isCompleted checks if Arc setup has been completed
func (a *installArcAction) isCompleted(ctx context.Context) bool {
	a.logger.Debug("Checking Arc setup completion status")

	// Check if Arc services are running
	if !isArcServicesRunning(ctx) {
		a.logger.Debug("Arc services are not running")
		return false
	}

	// Check azcmagent show with timeout
	timeoutCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(timeoutCtx, "azcmagent", "show")
	output, err := cmd.Output()
	if err != nil {
		a.logger.Debugf("azcmagent show failed: %v - Arc not ready", err)
		return false
	}

	// Parse output to check if agent is connected
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "Agent Status") && strings.Contains(line, ":") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				status := strings.TrimSpace(parts[1])
				isConnected := strings.ToLower(status) == "connected"
				if isConnected {
					a.logger.Debug("Arc setup appears to be completed - agent is connected")
				} else {
					a.logger.Debugf("Arc agent status is '%s' - not ready", status)
				}
				return isConnected
			}
		}
	}

	a.logger.Debug("Could not find Agent Status in azcmagent show output - Arc not ready")
	return false
}
