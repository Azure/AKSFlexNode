package kubeadm

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/tools/clientcmd/api/latest"
	"sigs.k8s.io/cluster-api/bootstrap/kubeadm/types/upstreamv1beta4"

	"go.goms.io/aks/AKSFlexNode/pkg/config"
	"go.goms.io/aks/AKSFlexNode/pkg/systemd"
	"go.goms.io/aks/AKSFlexNode/pkg/utils"
	"go.goms.io/aks/AKSFlexNode/pkg/utils/utilio"
)

type nodeJoinConfig struct {
	APIServerEndpoint string
	APIServerCAData   []byte

	KubeletAuthInfo   *api.AuthInfo
	KubeletNodeLabels map[string]string
	KubeletNodeIP     string
}

// nodeJoin provides the functionality for joining the current machine to
// the Kubernetes cluster using kubeadm.
// It expects the kubeadmin binary is present in the PATH.
type nodeJoin struct {
	logger         *logrus.Logger
	kubeadmCommand string // to allow overriding in unit test
	baseDir        string // base directory for the join config
	config         nodeJoinConfig
	systemd        systemd.Manager
}

func NewNodeJoin(cfg *config.Config, logger *logrus.Logger) (*nodeJoin, error) {
	baseDir, err := os.MkdirTemp("", "aks-flex-node-kubeadm-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir for kubeadm join config: %w", err)
	}

	ca, err := base64.StdEncoding.DecodeString(cfg.Node.Kubelet.CACertData)
	if err != nil {
		return nil, fmt.Errorf("decode CA cert data: %w", err)
	}

	rv := &nodeJoin{
		logger:  logger,
		baseDir: baseDir,
		config: nodeJoinConfig{
			APIServerEndpoint: cfg.Node.Kubelet.ServerURL,
			APIServerCAData:   ca,
			KubeletAuthInfo: &api.AuthInfo{
				Token: cfg.Azure.BootstrapToken.Token,
			},
		},
		systemd: systemd.New(),
	}

	return rv, nil
}

func (n *nodeJoin) resolveKubeadmBinary() (string, error) {
	if n.kubeadmCommand != "" {
		return n.kubeadmCommand, nil
	}

	return exec.LookPath("kubeadm")
}

func (n *nodeJoin) GetName() string {
	return "kubeadm-join"
}

func (n *nodeJoin) IsCompleted(ctx context.Context) bool {
	// if the kubelet directory exists,
	// we assume the node has already joined or is in the process of joining
	return hasDir(dirVarLibKubelet)
}

func (n *nodeJoin) Execute(ctx context.Context) error {
	if err := n.ensureBaseDir(); err != nil {
		return fmt.Errorf("ensure base dir: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(n.baseDir); err != nil {
			n.logger.Warnf("remove base dir: %s", err)
		}
	}()

	kubeadmBinary, err := n.resolveKubeadmBinary()
	if err != nil {
		return fmt.Errorf("resolve kubeadm binary: %w", err)
	}

	config, err := n.writeKubeadmJoinConfig()
	if err != nil {
		return fmt.Errorf("write kubeadm config: %w", err)
	}

	if err := n.ensureKubeletUnit(ctx); err != nil {
		return fmt.Errorf("ensure kubelet systemd unit: %w", err)
	}

	if err := utils.RunSystemCommand(
		kubeadmBinary,
		"join",
		"--config", config,
		"-v", "5",
	); err != nil {
		return fmt.Errorf("kubeadm join: %w", err)
	}

	if err := n.pollUntilKubeletActive(ctx); err != nil {
		return err
	}

	return nil
}

func (n *nodeJoin) ensureBaseDir() error {
	if hasDir(n.baseDir) {
		return nil
	}
	return os.MkdirAll(n.baseDir, 0700)
}

func (n *nodeJoin) writeFile(filename string, content []byte) (string, error) {
	const filePerm = 0600 // read/write for owner only

	p := filepath.Join(n.baseDir, filename)

	if err := utilio.WriteFile(p, content, filePerm); err != nil {
		return "", err
	}

	return p, nil
}

func (n *nodeJoin) writeBootstrapKubeconfig() (string, error) {
	const (
		cluster  = "cluster"
		context  = "context"
		authInfo = "user"
	)

	content, err := runtime.Encode(latest.Codec, &api.Config{
		Clusters: map[string]*api.Cluster{
			cluster: {
				CertificateAuthorityData: n.config.APIServerCAData,
				Server:                   n.config.APIServerEndpoint,
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
			authInfo: n.config.KubeletAuthInfo,
		},
	})
	if err != nil {
		return "", err
	}

	return n.writeFile("bootstrap.kubeconfig", content)
}

func (n *nodeJoin) writeKubeadmJoinConfig() (string, error) {
	bootstrapKubeconfig, err := n.writeBootstrapKubeconfig()
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
	if l := n.config.KubeletNodeLabels; len(l) > 0 {
		kubeletArgs = append(kubeletArgs, upstreamv1beta4.Arg{
			Name:  "node-labels",
			Value: nodeLabels(l),
		})
	}

	// Add --node-ip if configured (to advertise a different node IP)
	if n.config.KubeletNodeIP != "" {
		kubeletArgs = append(kubeletArgs, upstreamv1beta4.Arg{
			Name:  "node-ip",
			Value: n.config.KubeletNodeIP,
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

	return n.writeFile("join-config.yaml", content)
}

func (n *nodeJoin) ensureKubeletUnit(ctx context.Context) error {
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

	return nil
}

func (n *nodeJoin) pollUntilKubeletActive(ctx context.Context) error {
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
					n.logger.Debugf("kubelet unit status: %s", unit.ActiveState)
				}
				return active, nil
			}
		},
	)
}

func nodeLabels(labels map[string]string) string {
	kv := make([]string, 0, len(labels))

	for k, v := range labels {
		kv = append(kv, k+"="+v)
	}

	return strings.Join(kv, ",")
}

// hasDir checks if the given path exists and is a directory.
func hasDir(path string) bool {
	s, err := os.Stat(path)
	return err == nil && s.IsDir()
}
