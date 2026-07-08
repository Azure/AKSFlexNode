package aksmachine

import (
	"context"
	"fmt"
	"maps"
	"math"
	"slices"

	"github.com/Azure/AKSFlexNode/pkg/config"
)

// GoalState is the local agent representation of ARM machine desired settings.
// Keep this type independent from the public Azure SDK shape; adapt the SDK
// payload to this model when the ARM contract is finalized.
type GoalState struct {
	KubernetesVersion string            `json:"kubernetesVersion,omitempty"`
	SettingsVersion   string            `json:"settingsVersion,omitempty"`
	MaxPods           int               `json:"maxPods,omitempty"`
	NodeLabels        map[string]string `json:"nodeLabels,omitempty"`
	NodeTaints        []string          `json:"nodeTaints,omitempty"`
	KubeletConfig     KubeletConfig     `json:"kubeletConfig"`
}

type KubeletConfig struct {
	ImageGCHighThreshold int `json:"imageGCHighThreshold,omitempty"`
	ImageGCLowThreshold  int `json:"imageGCLowThreshold,omitempty"`
}

func (g GoalState) validate() error {
	if g.KubernetesVersion == "" {
		return fmt.Errorf("kubernetes version is empty")
	}
	if g.MaxPods < 0 {
		return fmt.Errorf("max pods must be non-negative")
	}
	if g.MaxPods > math.MaxInt32 {
		return fmt.Errorf("max pods must be less than or equal to %d", math.MaxInt32)
	}
	if g.KubeletConfig.ImageGCHighThreshold < 0 {
		return fmt.Errorf("image GC high threshold must be non-negative")
	}
	if g.KubeletConfig.ImageGCLowThreshold < 0 {
		return fmt.Errorf("image GC low threshold must be non-negative")
	}
	return nil
}

// GoalStateFromConfig builds and validates the initial AKS machine goal state
// from local agent configuration.
func GoalStateFromConfig(cfg *config.Config) (GoalState, error) {
	// Until the finalized Machine API exposes a settings version in all paths,
	// use KubernetesVersion as the same stable fallback used by ARM reads.
	goal := GoalState{
		KubernetesVersion: cfg.Components.Kubernetes,
		SettingsVersion:   cfg.Components.Kubernetes,
		MaxPods:           cfg.Node.MaxPods,
		NodeLabels:        maps.Clone(cfg.Node.Labels),
		NodeTaints:        slices.Clone(cfg.Node.Taints),
		KubeletConfig: KubeletConfig{
			ImageGCHighThreshold: cfg.Node.Kubelet.ImageGCHighThreshold,
			ImageGCLowThreshold:  cfg.Node.Kubelet.ImageGCLowThreshold,
		},
	}
	if err := goal.validate(); err != nil {
		return GoalState{}, err
	}
	return goal, nil
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
