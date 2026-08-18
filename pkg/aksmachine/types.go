package aksmachine

import (
	"context"
	"fmt"
	"maps"
	"math"
	"slices"

	"github.com/Azure/AKSFlexNode/pkg/config"
)

// MachineGoal preserves optional settings returned by the ARM Machine API.
// Pointer fields distinguish an omitted value from an explicit zero.
type MachineGoal struct {
	KubernetesVersion string               `json:"kubernetesVersion,omitempty"`
	SettingsVersion   string               `json:"settingsVersion,omitempty"`
	MaxPods           *int                 `json:"maxPods,omitempty"`
	NodeLabels        map[string]string    `json:"nodeLabels,omitempty"`
	NodeTaints        []string             `json:"nodeTaints,omitempty"`
	KubeletConfig     MachineKubeletConfig `json:"kubeletConfig"`
}

type MachineKubeletConfig struct {
	ImageGCHighThreshold *int `json:"imageGCHighThreshold,omitempty"`
	ImageGCLowThreshold  *int `json:"imageGCLowThreshold,omitempty"`
}

// Validate verifies every scalar present in a Machine goal while allowing the
// API to omit values that the agent will fill from local configuration.
func (g MachineGoal) Validate() error {
	if g.KubernetesVersion == "" {
		return fmt.Errorf("kubernetes version is empty")
	}
	if g.MaxPods != nil {
		if *g.MaxPods <= 0 {
			return fmt.Errorf("max pods must be positive")
		}
		if *g.MaxPods > math.MaxInt32 {
			return fmt.Errorf("max pods must be less than or equal to %d", math.MaxInt32)
		}
	}
	if g.KubeletConfig.ImageGCHighThreshold != nil {
		if *g.KubeletConfig.ImageGCHighThreshold <= 0 {
			return fmt.Errorf("image GC high threshold must be positive")
		}
		if *g.KubeletConfig.ImageGCHighThreshold > 100 {
			return fmt.Errorf("image GC high threshold must be less than or equal to 100")
		}
	}
	if g.KubeletConfig.ImageGCLowThreshold != nil {
		if *g.KubeletConfig.ImageGCLowThreshold < 0 {
			return fmt.Errorf("image GC low threshold must be non-negative")
		}
		if *g.KubeletConfig.ImageGCLowThreshold > 100 {
			return fmt.Errorf("image GC low threshold must be less than or equal to 100")
		}
	}
	if g.KubeletConfig.ImageGCHighThreshold != nil && g.KubeletConfig.ImageGCLowThreshold != nil &&
		*g.KubeletConfig.ImageGCLowThreshold >= *g.KubeletConfig.ImageGCHighThreshold {
		return fmt.Errorf("image GC low threshold must be less than image GC high threshold")
	}
	return nil
}

// GoalState contains the complete effective settings used to render a node.
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

// Validate verifies that a goal is complete and can be rendered on a node.
// SettingsVersion is validated by Machine because a local bootstrap goal does
// not have an ETag until it is persisted.
func (g GoalState) Validate() error {
	return (MachineGoal{
		KubernetesVersion: g.KubernetesVersion,
		MaxPods:           &g.MaxPods,
		KubeletConfig: MachineKubeletConfig{
			ImageGCHighThreshold: &g.KubeletConfig.ImageGCHighThreshold,
			ImageGCLowThreshold:  &g.KubeletConfig.ImageGCLowThreshold,
		},
	}).Validate()
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
	if err := goal.Validate(); err != nil {
		return GoalState{}, err
	}
	return goal, nil
}

// DeepCopy returns a goal whose mutable fields are independent of the source.
func (g GoalState) DeepCopy() *GoalState {
	cloned := g
	cloned.NodeLabels = maps.Clone(g.NodeLabels)
	cloned.NodeTaints = slices.Clone(g.NodeTaints)
	return &cloned
}

// EffectiveGoal overlays a Machine goal on a complete local goal. AKS owns the
// desired values; the local goal only fills scalar fields omitted by the API.
func EffectiveGoal(machine MachineGoal, local GoalState) (GoalState, error) {
	effective := GoalState{
		KubernetesVersion: machine.KubernetesVersion,
		SettingsVersion:   machine.SettingsVersion,
		MaxPods:           local.MaxPods,
		NodeLabels:        maps.Clone(machine.NodeLabels),
		NodeTaints:        slices.Clone(machine.NodeTaints),
		KubeletConfig:     local.KubeletConfig,
	}
	if machine.MaxPods != nil {
		effective.MaxPods = *machine.MaxPods
	}
	if machine.KubeletConfig.ImageGCHighThreshold != nil {
		effective.KubeletConfig.ImageGCHighThreshold = *machine.KubeletConfig.ImageGCHighThreshold
	}
	if machine.KubeletConfig.ImageGCLowThreshold != nil {
		effective.KubeletConfig.ImageGCLowThreshold = *machine.KubeletConfig.ImageGCLowThreshold
	}
	if err := effective.Validate(); err != nil {
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
	ID     string      `json:"id,omitempty"`
	Name   string      `json:"name,omitempty"`
	Goal   MachineGoal `json:"goal"`
	Status Status      `json:"status"`
}

// Validate verifies the required fields and every scalar present in a Machine
// returned by AKS. Omitted scalars are validated after local defaults apply.
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
