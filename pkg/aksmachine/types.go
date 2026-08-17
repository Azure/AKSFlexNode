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

// Validate verifies the values present in a goal. SettingsVersion is validated
// by Machine because local bootstrap goals do not have an ETag until persisted.
func (g GoalState) Validate() error {
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
	if g.KubeletConfig.ImageGCHighThreshold > 100 {
		return fmt.Errorf("image GC high threshold must be less than or equal to 100")
	}
	if g.KubeletConfig.ImageGCLowThreshold > 100 {
		return fmt.Errorf("image GC low threshold must be less than or equal to 100")
	}
	if g.KubeletConfig.ImageGCHighThreshold != 0 && g.KubeletConfig.ImageGCLowThreshold >= g.KubeletConfig.ImageGCHighThreshold {
		return fmt.Errorf("image GC low threshold must be less than image GC high threshold")
	}
	return nil
}

// ValidateEffective verifies that omitted API defaults have been resolved and
// the goal has every scalar setting needed to render a node.
func (g GoalState) ValidateEffective() error {
	if err := g.Validate(); err != nil {
		return err
	}
	if g.MaxPods == 0 {
		return fmt.Errorf("max pods is empty")
	}
	if g.KubeletConfig.ImageGCHighThreshold == 0 {
		return fmt.Errorf("image GC high threshold is empty")
	}
	return nil
}

// GoalStateFromConfig builds and validates the initial AKS machine goal state
// from local agent configuration.
func GoalStateFromConfig(cfg *config.Config) (GoalState, error) {
	goal := GoalState{
		KubernetesVersion: cfg.Components.Kubernetes,
		MaxPods:           cfg.Node.MaxPods,
		NodeLabels:        maps.Clone(cfg.Node.Labels),
		NodeTaints:        slices.Clone(cfg.Node.Taints),
		KubeletConfig: KubeletConfig{
			ImageGCHighThreshold: cfg.Node.Kubelet.ImageGCHighThreshold,
			ImageGCLowThreshold:  cfg.Node.Kubelet.ImageGCLowThreshold,
		},
	}
	if err := goal.ValidateEffective(); err != nil {
		return GoalState{}, err
	}
	return goal, nil
}

func cloneGoalState(goal GoalState) GoalState {
	cloned := goal
	cloned.NodeLabels = maps.Clone(goal.NodeLabels)
	cloned.NodeTaints = slices.Clone(goal.NodeTaints)
	return cloned
}

// EffectiveGoal overlays a Machine goal on a complete local goal. AKS owns the
// desired values; the local goal only fills scalar fields omitted by the API.
func EffectiveGoal(machine, local GoalState) (GoalState, error) {
	effective := cloneGoalState(machine)
	if effective.MaxPods == 0 {
		effective.MaxPods = local.MaxPods
	}
	if effective.KubeletConfig.ImageGCHighThreshold == 0 {
		effective.KubeletConfig.ImageGCHighThreshold = local.KubeletConfig.ImageGCHighThreshold
	}
	if effective.KubeletConfig.ImageGCLowThreshold == 0 {
		effective.KubeletConfig.ImageGCLowThreshold = local.KubeletConfig.ImageGCLowThreshold
	}
	if err := effective.ValidateEffective(); err != nil {
		return GoalState{}, fmt.Errorf("validate effective goal: %w", err)
	}
	return effective, nil
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

// Validate verifies that a Machine returned by AKS contains a goal suitable
// for bootstrap or reconciliation.
func (m *Machine) Validate() error {
	if m == nil {
		return fmt.Errorf("machine is nil")
	}
	if err := m.Goal.Validate(); err != nil {
		return fmt.Errorf("goal: %w", err)
	}
	if m.Goal.SettingsVersion == "" {
		return fmt.Errorf("goal settings version is empty")
	}
	return nil
}

// MachineClient provides access to the AKS-side machine representation.
// Production should use the official Azure SDK implementation once the public
// SDK contains the finalized resource shape; tests can provide fake or remote
// implementations of this interface.
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
