package kubeadm

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
	"sigs.k8s.io/cluster-api/bootstrap/kubeadm/types/upstreamv1beta4"

	v20260301 "go.goms.io/aks/AKSFlexNode/components/kubeadm/v20260301"
	"go.goms.io/aks/AKSFlexNode/components/services/actions"
	"go.goms.io/aks/AKSFlexNode/pkg/systemd"
	"go.goms.io/aks/AKSFlexNode/pkg/utils"
	"go.goms.io/aks/AKSFlexNode/pkg/utils/utilio"
	"go.goms.io/aks/AKSFlexNode/pkg/utils/utilpb"
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
	config, err := utilpb.AnyTo[*v20260301.KubadmNodeJoin](req.GetItem())
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
	config *v20260301.KubeadmNodeJoinSpec,
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

func (n *nodeJoinAction) writeKubeadmJoinConfig(
	baseDir string,
	config *v20260301.KubeadmNodeJoinSpec,
) (string, error) {
	bootstrapKubeconfig, err := n.writeBootstrapKubeconfig(baseDir, config)
	if err != nil {
		return "", err
	}

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
	if l := config.GetKubelet().GetNodeLabels(); len(l) > 0 {
		kubeletArgs = append(kubeletArgs, upstreamv1beta4.Arg{
			Name:  "node-labels",
			Value: nodeLabels(l),
		})
	}

	// Add --node-ip if configured (to advertise a different node IP)
	if config.GetKubelet().HasNodeIp() {
		kubeletArgs = append(kubeletArgs, upstreamv1beta4.Arg{
			Name:  "node-ip",
			Value: config.GetKubelet().GetNodeIp(),
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
	_, err := n.systemd.GetUnitStatus(ctx, systemdUnitKubelet)
	switch {
	case errors.Is(err, systemd.ErrUnitNotFound):
		// proceed to create
	case err != nil:
		return err
	default:
		return nil // unit already exists, nothing to do
	}

	if err := n.systemd.WriteUnitFile(
		ctx,
		systemdUnitKubelet,
		systemdUnitKubeletFile,
	); err != nil {
		return fmt.Errorf("kubelet unit: %w", err)
	}
	if err := n.systemd.WriteDropInFile(
		ctx,
		systemdUnitKubelet,
		systemdDropInKubeadm,
		systemdDropInKubeadmFile,
	); err != nil {
		return fmt.Errorf("kubelet unit drop-in: %w", err)
	}

	if err := n.systemd.DaemonReload(ctx); err != nil {
		return fmt.Errorf("systemd daemon reload: %w", err)
	}

	if err := n.systemd.EnableUnit(ctx, systemdUnitKubelet); err != nil {
		return fmt.Errorf("enable kubelet unit: %w", err)
	}

	return nil
}

func (n *nodeJoinAction) canRun() bool {
	// if the kubelet directory exists,
	// we assume the node has already joined or is in the process of joining
	return !hasDir(dirVarLibKubelet)
}

func (n *nodeJoinAction) runJoin(
	ctx context.Context,
	config *v20260301.KubeadmNodeJoinSpec,
) error {
	baseDir, err := os.MkdirTemp("", "aks-flex-node-kubeadm-*") // maybe move to utilio?
	if err != nil {
		return status.Errorf(codes.Internal, "create temp dir for kubeadm join config: %s", err)
	}
	_ = os.RemoveAll(baseDir) //nolint:errcheck // clean up temp dir

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

	if err := utils.RunSystemCommand(
		kubeadmCommand,
		"join", "--config", joinConfig, "-v", "5",
	); err != nil {
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
			unit, err := n.systemd.GetUnitStatus(ctx, systemdUnitKubelet)
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
				if !active {
					// n.logger.Debugf("kubelet unit status: %s", unit.ActiveState)
				}
				return active, nil
			}
		},
	)
}

func hasDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func nodeLabels(labels map[string]string) string {
	parts := make([]string, 0, len(labels))
	for k, v := range labels {
		parts = append(parts, fmt.Sprintf("%s=%s", k, v))
	}

	return strings.Join(parts, ",")
}
