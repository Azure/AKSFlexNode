package daemon

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/daemon/internal/noneops"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	corev1 "k8s.io/api/core/v1"
)

const noneGenerationLabel = "kubernetes.azure.com/flex-runtime-generation"

func noneRegistrationState(state *State) *State {
	frozen := *state
	frozen.NodeConfig = state.NodeConfig.DeepCopy()
	if frozen.NodeConfig.Kubelet.Labels == nil {
		frozen.NodeConfig.Kubelet.Labels = make(map[string]string)
	}
	// A source restarted after rollback is a new registration too, even
	// though its AKS settings version is unchanged.
	sum := sha256.Sum256([]byte(state.AppliedSettingsVersion + ":" + rand.Text()))
	frozen.NodeConfig.Kubelet.Labels[noneGenerationLabel] = hex.EncodeToString(sum[:16])
	return &frozen
}

func noneNPDConfig(state *State) *config.Config {
	cfg := &config.Config{Npd: config.NPDConfig{Version: state.NPDVersion}}
	if offline := state.NodeConfig.OfflineArtifacts; offline != nil {
		cfg.Bootstrap.OfflineArtifacts.Source = offline.Source
	}
	return cfg
}

// ResumeRepave completes the frozen operation, never the latest RP goal. A
// startup remains pending until its recreated Node has been observed; otherwise
// the same absence could incorrectly authorize a second destructive repave.
func (o *noneNodeOperator) ResumeRepave(ctx context.Context, log *slog.Logger, observed *corev1.Node) (bool, error) {
	state, err := o.applied(ctx)
	if err != nil {
		return false, err
	}
	source := noneRegistrationState(state)
	sourceCompletion, err := json.Marshal(source)
	if err != nil {
		return false, err
	}
	completion, err := json.Marshal(state)
	if err != nil {
		return false, err
	}
	node := o.newNode(noneops.Params{
		Log: log, Config: state.NodeConfig, Generation: state.AppliedSettingsVersion,
		ArtifactVersion: state.NPDVersion, Completion: completion,
		Previous: source.NodeConfig, PreviousGeneration: source.AppliedSettingsVersion,
		PreviousArtifactVersion: source.NPDVersion, PreviousCompletion: sourceCompletion,
	})
	startup, err := node.PendingStartup()
	if err != nil {
		return true, err
	}
	if startup == nil {
		pending, err := node.PendingUpgrade()
		if err != nil || !pending {
			return pending, err
		}
		if observed != nil {
			return true, fmt.Errorf("interrupted none preparation requires a fresh RP Node deletion before destructive recovery")
		}
		if err := node.RecoverUpgrade(ctx); err != nil {
			return true, err
		}
		startup, err = node.PendingStartup()
		if err != nil {
			return true, err
		}
		if startup == nil {
			return true, fmt.Errorf("none recovery did not publish startup intent")
		}
	}
	var frozen State
	if err := json.Unmarshal(startup.Completion, &frozen); err != nil {
		return true, fmt.Errorf("decode frozen none completion: %w", err)
	}
	if frozen.Isolation != isolation.ModeNone || frozen.NodeConfig == nil ||
		frozen.AppliedSettingsVersion != startup.Generation || frozen.NPDVersion != startup.ArtifactVersion ||
		!reflect.DeepEqual(frozen.NodeConfig, startup.Config) {
		return true, fmt.Errorf("none startup and frozen applied state disagree")
	}
	node = o.newNode(noneops.Params{
		Log: log, Config: frozen.NodeConfig, Generation: frozen.AppliedSettingsVersion,
		ArtifactVersion: frozen.NPDVersion,
	})
	if err := node.ResumeStartup(ctx); err != nil {
		return true, err
	}
	if !startup.Finished {
		gs, err := node.GoalState()
		if err != nil {
			return true, err
		}
		if err := o.startNPD(ctx, log, noneNPDConfig(&frozen), gs.NodeStart); err != nil {
			return true, err
		}
		if err := o.state.Save(ctx, &frozen); err != nil {
			return true, fmt.Errorf("persist frozen none state: %w", err)
		}
		if err := node.Commit(); err != nil {
			return true, err
		}
		if err := node.FinishStartup(); err != nil {
			return true, err
		}
	}
	expected := frozen.NodeConfig.Kubelet.Labels[noneGenerationLabel]
	if observed != nil && expected != "" && observed.Labels[noneGenerationLabel] == expected {
		if err := node.ClearStartup(); err != nil {
			return true, err
		}
	}
	return true, nil
}
