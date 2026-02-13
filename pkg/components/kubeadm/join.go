package kubeadm

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/tools/clientcmd/api/latest"
	"sigs.k8s.io/cluster-api/bootstrap/kubeadm/types/upstreamv1beta4"

	"go.goms.io/aks/AKSFlexNode/pkg/utils"
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
	kubeadmCommand string // to allow overriding in unit test
	baseDir        string // base directory for the join config
	config         nodeJoinConfig
}

func (n *nodeJoin) GetName() string {
	return "kubeadm-join"
}

func (n *nodeJoin) IsCompleted(ctx context.Context) bool {
	return n.pollForKubeletStatus(ctx) == nil
}

func (n *nodeJoin) Execute(ctx context.Context) error {
	config, err := n.writeKubeadmJoinConfig(ctx)
	if err != nil {
		return fmt.Errorf("write kubeadm config: %w", err)
	}

	if err := utils.RunSystemCommand(
		n.kubeadmCommand,
		"join",
		"--config", config,
		"--v", "5",
	); err != nil {
		return fmt.Errorf("kubeadm join: %w", err)
	}

	if err := n.pollForKubeletStatus(ctx); err != nil {
		return err
	}

	return nil
}

func (n *nodeJoin) writeFile(filename string, content []byte) (string, error) {
	const filePerm = 0600 // read/write for owner only

	p := filepath.Join(n.baseDir, filename)

	if err := os.WriteFile(p, content, filePerm); err != nil {
		return "", err
	}

	return p, nil
}

func (n *nodeJoin) writeBootstrapKubeconfig(ctx context.Context) (string, error) {
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

func (n *nodeJoin) writeKubeadmJoinConfig(
	ctx context.Context,
) (string, error) {
	bootstrapKubeconfig, err := n.writeBootstrapKubeconfig(ctx)
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

func (n *nodeJoin) pollForKubeletStatus(ctx context.Context) error {
	// TODO: check for kubelet systemd unit status
	return nil
}

func nodeLabels(labels map[string]string) string {
	kv := make([]string, 0, len(labels))

	for k, v := range labels {
		kv = append(kv, k+"="+v)
	}

	return strings.Join(kv, ",")
}
