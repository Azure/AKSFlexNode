package daemon

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/config"
	corev1 "k8s.io/api/core/v1"
)

const noneGenerationLabel = "kubernetes.azure.com/flex-runtime-generation"

func noneRegistrationState(state *State) *State {
	frozen := *state
	frozen.NodeConfig = state.NodeConfig.DeepCopy()
	if frozen.NodeConfig.Kubelet.Labels == nil {
		frozen.NodeConfig.Kubelet.Labels = make(map[string]string)
	}
	// Reapplying the same AKS settings still creates a fresh registration.
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

// ResumeRepave runs the SDK's applied-config recovery once before normal RP
// reconciliation. Recovery may roll an uncommitted target back to the source.
func (o *noneNodeOperator) ResumeRepave(ctx context.Context, log *slog.Logger, _ *corev1.Node) (bool, error) {
	return false, o.recover(ctx, log)
}
