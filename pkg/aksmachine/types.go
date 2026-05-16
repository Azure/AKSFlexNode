package aksmachine

import (
	"context"
	"fmt"
)

// GoalState is the local agent representation of ARM machine desired settings.
// Keep this type independent from the public Azure SDK shape; adapt the SDK
// payload to this model when the ARM contract is finalized.
type GoalState struct {
	KubernetesVersion string `json:"kubernetesVersion,omitempty"`
	SettingsVersion   string `json:"settingsVersion,omitempty"`
}

type ProvisioningState string

const (
	ProvisioningStatePending     ProvisioningState = "Pending"
	ProvisioningStateReconciling ProvisioningState = "Reconciling"
	ProvisioningStateSucceeded   ProvisioningState = "Succeeded"
	ProvisioningStateFailed      ProvisioningState = "Failed"
	ProvisioningStateDeleting    ProvisioningState = "Deleting"
)

// Status is the local agent representation of ARM machine status.
type Status struct {
	ProvisioningState       ProvisioningState `json:"provisioningState,omitempty"`
	ObservedSettingsVersion string            `json:"observedSettingsVersion,omitempty"`
	Message                 string            `json:"message,omitempty"`
}

// Machine is the local agent representation of the AKS RP machine resource.
type Machine struct {
	ID     string    `json:"id,omitempty"`
	Name   string    `json:"name,omitempty"`
	Goal   GoalState `json:"goal"`
	Status Status    `json:"status"`
}

// MachineClient provides access to the AKS-side machine representation.
// Production should use the official Azure SDK implementation once the public
// SDK contains the finalized resource shape; e2e tests can provide a local or
// remote implementation of this interface.
type MachineClient interface {
	Create(ctx context.Context, desired GoalState) (*Machine, error)
	Get(ctx context.Context) (*Machine, error)
	PatchStatus(ctx context.Context, status Status) error
}

// NotFoundError is returned when the ARM machine resource does not exist.
type NotFoundError struct {
	Resource string
}

func (e *NotFoundError) Error() string {
	if e == nil || e.Resource == "" {
		return "machine resource not found"
	}
	return fmt.Sprintf("machine resource %q not found", e.Resource)
}
