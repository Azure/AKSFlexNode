package v20260301

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/tools/clientcmd/api/latest"
	utilexec "k8s.io/utils/exec"
	"sigs.k8s.io/cluster-api/bootstrap/kubeadm/types/upstreamv1beta4"

	"github.com/Azure/AKSFlexNode/components/kubeadm"
	"github.com/Azure/AKSFlexNode/components/services/actions"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/systemd"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilpb"
)

type nodeJoinAction struct {
	systemd        systemd.Manager
	kubeadmCommand string // to allow overriding in unit test
}

func newNodeJoinAction() (actions.Server, error) {
	systemdManager := systemd.New()

	return &nodeJoinAction{
		systemd: systemdManager,
	}, nil
}

var _ actions.Server = (*nodeJoinAction)(nil)

func (n *nodeJoinAction) ApplyAction(
	ctx context.Context,
	req *actions.ApplyActionRequest,
) (*actions.ApplyActionResponse, error) {
	config, err := utilpb.AnyTo[*kubeadm.KubeadmNodeJoin](req.GetItem())
	if err != nil {
		return nil, err
	}

	if n.canRun() {
		if err := n.runJoin(ctx, config.GetSpec()); err != nil {
			return nil, err
		}
	}

	if err := n.pollUntilKubeletActive(ctx); err != nil {
		return nil, err
	}

	// TODO: capture status
	item, err := anypb.New(config)
	if err != nil {
		return nil, err
	}

	return actions.ApplyActionResponse_builder{Item: item}.Build(), nil
}

func (n *nodeJoinAction) resolveKubeadmBinary() (string, error) {
	if n.kubeadmCommand != "" {
		return n.kubeadmCommand, nil
	}

	return exec.LookPath("kubeadm")
}

func (n *nodeJoinAction) writeBootstrapKubeconfig(
	baseDir string,
	config *kubeadm.KubeadmNodeJoinSpec,
) (string, error) {
	const (
		cluster  = "cluster"
		context  = "context"
		authInfo = "user"
	)

	content, err := runtime.Encode(latest.Codec, &api.Config{
		Clusters: map[string]*api.Cluster{
			cluster: {
				CertificateAuthorityData: config.GetControlPlane().GetCertificateAuthorityData(),
				Server:                   config.GetControlPlane().GetServer(),
			},
		},
		Contexts: map[string]*api.Context{
			context: {
				Cluster:  cluster,
				AuthInfo: authInfo,
			},
		},
		CurrentContext: context,
		AuthInfos: map[string]*api.AuthInfo{
			authInfo: {
				// TODO: add support for exec plugin
				Token: config.GetKubelet().GetBootstrapAuthInfo().GetToken(),
			},
		},
	})
	if err != nil {
		return "", err
	}

	dest := filepath.Join(baseDir, "bootstrap.kubeconfig")
	if err := utilio.WriteFile(dest, content, 0600); err != nil {
		return "", err
	}

	return dest, nil
}

func (n *nodeJoinAction) ensureRuntimeFolders() error {
	// create static pod dir -- this dir is not managed by kubeadm as we don't create any static pod
	// ref: https://github.com/kubernetes/kubernetes/blob/147a9ee31545188a1a53ad64ed12add16e95f04a/cmd/kubeadm/app/util/staticpod/utils.go#L191-L192
	if err := os.MkdirAll(config.KubeletStaticPodPath, 0700); err != nil {
		return nil
	}

	return nil
}

func (n *nodeJoinAction) writeKubeadmJoinConfig(
	baseDir string,
	config *kubeadm.KubeadmNodeJoinSpec,
) (string, error) {
	bootstrapKubeconfig, err := n.writeBootstrapKubeconfig(baseDir, config)
	if err != nil {
		return "", err
	}

	kubeletConfig := config.GetKubelet()

	scheme := runtime.NewScheme()
	scheme.AddKnownTypes(upstreamv1beta4.GroupVersion,
		&upstreamv1beta4.JoinConfiguration{},
	)
	codec := serializer.NewCodecFactory(scheme).CodecForVersions(
		json.NewYAMLSerializer(json.DefaultMetaFactory, scheme, scheme),
		nil,
		schema.GroupVersions{upstreamv1beta4.GroupVersion},
		nil,
	)

	// Build kubelet extra args
	var kubeletArgs []upstreamv1beta4.Arg

	// Add static node labels
	if l := kubeletConfig.GetNodeLabels(); len(l) > 0 {
		kubeletArgs = append(kubeletArgs, upstreamv1beta4.Arg{
			Name:  "node-labels",
			Value: nodeLabels(l),
		})
	}

	// Add --node-ip if configured (to advertise a different node IP)
	if nodeIP := kubeletConfig.GetNodeIp(); nodeIP != "" {
		kubeletArgs = append(kubeletArgs, upstreamv1beta4.Arg{
			Name:  "node-ip",
			Value: nodeIP,
		})
	}

	content, err := runtime.Encode(codec, &upstreamv1beta4.JoinConfiguration{
		Discovery: upstreamv1beta4.Discovery{
			File: &upstreamv1beta4.FileDiscovery{
				KubeConfigPath: bootstrapKubeconfig,
			},
		},
		NodeRegistration: upstreamv1beta4.NodeRegistrationOptions{
			KubeletExtraArgs: kubeletArgs,
			Taints:           kubeletConfig.GetK8SRegisterTaints(),
		},
	})
	if err != nil {
		return "", err
	}

	dest := filepath.Join(baseDir, "join-config.yaml")
	if err := utilio.WriteFile(dest, content, 0600); err != nil {
		return "", err
	}

	return dest, nil
}

func (n *nodeJoinAction) ensureKubeletUnit(ctx context.Context) error {
	unitUpdated, err := n.systemd.EnsureUnitFile(
		ctx,
		config.SystemdUnitKubelet,
		systemdUnitKubeletFile,
	)
	if err != nil {
		return fmt.Errorf("kubelet unit: %w", err)
	}

	dropInUpdated, err := n.systemd.EnsureDropInFile(
		ctx,
		config.SystemdUnitKubelet,
		systemdDropInKubeadm,
		systemdDropInKubeadmFile,
	)
	if err != nil {
		return fmt.Errorf("kubelet unit drop-in: %w", err)
	}

	if unitUpdated || dropInUpdated {
		if err := n.systemd.DaemonReload(ctx); err != nil {
			return fmt.Errorf("systemd daemon reload: %w", err)
		}
	}

	if err := n.systemd.EnableUnit(ctx, config.SystemdUnitKubelet); err != nil {
		return fmt.Errorf("enable kubelet unit: %w", err)
	}

	return nil
}

func (n *nodeJoinAction) canRun() bool {
	// If kubelet config file exists, we assume the node has already joined or is in the process of joining.
	return !hasFile(filepath.Join(config.KubeletRoot, "config.yaml"))
}

func (n *nodeJoinAction) runJoin(
	ctx context.Context,
	config *kubeadm.KubeadmNodeJoinSpec,
) error {
	baseDir, err := os.MkdirTemp("", "aks-flex-node-kubeadm-*") // maybe move to utilio?
	if err != nil {
		return status.Errorf(codes.Internal, "create temp dir for kubeadm join config: %s", err)
	}
	defer func() {
		_ = os.RemoveAll(baseDir) //nolint:errcheck // clean up temp dir
	}()

	if err := n.ensureRuntimeFolders(); err != nil {
		return status.Errorf(codes.Internal, "ensure runtime folder: %s", err)
	}

	joinConfig, err := n.writeKubeadmJoinConfig(baseDir, config)
	if err != nil {
		return status.Errorf(codes.Internal, "write kubeadm config: %s", err)
	}

	if err := n.ensureKubeletUnit(ctx); err != nil {
		return status.Errorf(codes.Internal, "ensure kubelet systemd unit: %s", err)
	}

	kubeadmCommand, err := n.resolveKubeadmBinary()
	if err != nil {
		return status.Errorf(codes.Internal, "resolve kubeadm binary: %s", err)
	}

	if err := utilexec.New().CommandContext(ctx, kubeadmCommand, "join", "--config", joinConfig, "-v", "5").Run(); err != nil {
		return status.Errorf(codes.Internal, "kubeadm join: %s", err)
	}

	return nil
}

func (n *nodeJoinAction) pollUntilKubeletActive(ctx context.Context) error {
	const (
		pollInterval = 5 * time.Second
		waitTimeout  = 3 * time.Minute
	)

	return wait.PollUntilContextTimeout(
		ctx,
		pollInterval, waitTimeout, true,
		func(ctx context.Context) (bool, error) {
			unit, err := n.systemd.GetUnitStatus(ctx, config.SystemdUnitKubelet)
			switch {
			case errors.Is(err, systemd.ErrUnitNotFound):
				// If the unit is not found, it likely means the kubelet hasn't started yet,
				// so we return false to keep polling
				return false, nil
			case err != nil:
				// For any other error, we should return it to stop polling and surface the issue
				return false, err
			default:
				active := unit.ActiveState == systemd.UnitActiveStateActive
				// TODO: log kubelet unit status when it's not active
				return active, nil
			}
		},
	)
}

func hasFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func nodeLabels(labels map[string]string) string {
	parts := make([]string, 0, len(labels))
	for k, v := range labels {
		parts = append(parts, fmt.Sprintf("%s=%s", k, v))
	}

	return strings.Join(parts, ",")
}
