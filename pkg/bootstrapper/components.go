package bootstrapper

import (
	"context"
	"encoding/base64"
	"fmt"
	"maps"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"go.goms.io/aks/AKSFlexNode/components/api"
	"go.goms.io/aks/AKSFlexNode/components/cni"
	"go.goms.io/aks/AKSFlexNode/components/cri"
	"go.goms.io/aks/AKSFlexNode/components/kubebins"
	"go.goms.io/aks/AKSFlexNode/components/kubelet"
	"go.goms.io/aks/AKSFlexNode/components/linux"
	"go.goms.io/aks/AKSFlexNode/components/npd"
	"go.goms.io/aks/AKSFlexNode/components/services/actions"
	"go.goms.io/aks/AKSFlexNode/pkg/config"
)

// componentExecutor implemens the Executor interface for components api.
// TODO: move to a new package once we have migrated all bootstrappers.
type componentExecutor[M proto.Message] struct {
	Name          string
	ResolveAction func(name string, cfg *config.Config) (M, error)

	conn *grpc.ClientConn
	cfg  *config.Config
}

var _ Executor = (*componentExecutor[proto.Message])(nil)

func (c *componentExecutor[M]) Execute(ctx context.Context) error {
	action, err := c.ResolveAction(c.Name, c.cfg)
	if err != nil {
		return fmt.Errorf("resolve action: %w", err)
	}

	_, err = actions.ApplyAction(c.conn, ctx, action)
	return err
}

func (c *componentExecutor[M]) GetName() string {
	return c.Name
}

func (c *componentExecutor[M]) IsCompleted(ctx context.Context) bool {
	return false // delegate the idempotency check to the component api
}

type resolveActionFunc[M proto.Message] func(name string, cfg *config.Config) (M, error)

func (r resolveActionFunc[M]) Executor(name string, conn *grpc.ClientConn) Executor {
	return &componentExecutor[M]{
		Name:          name,
		ResolveAction: r,
		conn:          conn,
		cfg:           config.GetConfig(),
	}
}

func ptrWithDefault[T comparable](value T, defaultValue T) *T {
	var zero T

	if value == zero {
		return &defaultValue
	}

	return &value
}

func ptr[T any](value T) *T {
	return &value
}

func componentAction(name string) *api.Metadata {
	return api.Metadata_builder{Name: &name}.Build()
}

var configureSystem resolveActionFunc[*linux.ConfigureBaseOS] = func(
	name string,
	cfg *config.Config,
) (*linux.ConfigureBaseOS, error) {
	spec := linux.ConfigureBaseOSSpec_builder{}.Build()

	return linux.ConfigureBaseOS_builder{
		Metadata: componentAction(name),
		Spec:     spec,
	}.Build(), nil
}

var downloadCRIBinaries resolveActionFunc[*cri.DownloadCRIBinaries] = func(
	name string,
	cfg *config.Config,
) (*cri.DownloadCRIBinaries, error) {
	spec := cri.DownloadCRIBinariesSpec_builder{
		ContainerdVersion: ptrWithDefault(
			cfg.Containerd.Version,
			config.DefaultContainerdVersion,
		),
		RuncVersion: ptrWithDefault(
			cfg.Runc.Version,
			config.DefaultRunCVersion,
		),
	}.Build()

	return cri.DownloadCRIBinaries_builder{
		Metadata: componentAction(name),
		Spec:     spec,
	}.Build(), nil
}

var startContainerdService resolveActionFunc[*cri.StartContainerdService] = func(
	name string,
	cfg *config.Config,
) (*cri.StartContainerdService, error) {
	spec := cri.StartContainerdServiceSpec_builder{
		MetricsAddress: ptrWithDefault(
			cfg.Containerd.MetricsAddress,
			config.DefaultContainerdMetricsAddress,
		),
		SandboxImage: ptrWithDefault(
			cfg.Containerd.PauseImage,
			config.DefaultSandboxImage,
		),
	}.Build()

	return cri.StartContainerdService_builder{
		Metadata: componentAction(name),
		Spec:     spec,
	}.Build(), nil
}

var downloadKubeBinaries resolveActionFunc[*kubebins.DownloadKubeBinaries] = func(
	name string,
	cfg *config.Config,
) (*kubebins.DownloadKubeBinaries, error) {
	spec := kubebins.DownloadKubeBinariesSpec_builder{
		KubernetesVersion: ptr(cfg.Kubernetes.Version),
	}.Build()

	return kubebins.DownloadKubeBinaries_builder{
		Metadata: componentAction(name),
		Spec:     spec,
	}.Build(), nil
}

var downloadCNIBinaries resolveActionFunc[*cni.DownloadCNIBinaries] = func(
	name string,
	cfg *config.Config,
) (*cni.DownloadCNIBinaries, error) {
	spec := cni.DownloadCNIBinariesSpec_builder{
		CniPluginsVersion: ptrWithDefault(
			cfg.CNI.Version,
			config.DefaultCNIPluginsVersion,
		),
	}.Build()

	return cni.DownloadCNIBinaries_builder{
		Metadata: componentAction(name),
		Spec:     spec,
	}.Build(), nil
}

var downloadNPD resolveActionFunc[*npd.DownloadNodeProblemDetector] = func(
	name string,
	cfg *config.Config,
) (*npd.DownloadNodeProblemDetector, error) {
	spec := npd.DownloadNodeProblemDetectorSpec_builder{
		Version: ptrWithDefault(
			cfg.Npd.Version,
			config.DefaultNPDVersion,
		),
	}.Build()

	return npd.DownloadNodeProblemDetector_builder{
		Metadata: componentAction(name),
		Spec:     spec,
	}.Build(), nil
}

var startNPD resolveActionFunc[*npd.StartNodeProblemDetector] = func(
	name string,
	cfg *config.Config,
) (*npd.StartNodeProblemDetector, error) {
	spec := npd.StartNodeProblemDetectorSpec_builder{
		ApiServer:      ptr(cfg.Node.Kubelet.ServerURL),
		KubeConfigPath: ptr("/var/lib/kubelet/kubeconfig"), // FIXME: use a const
	}.Build()

	return npd.StartNodeProblemDetector_builder{
		Metadata: componentAction(name),
		Spec:     spec,
	}.Build(), nil
}

var startKubelet resolveActionFunc[*kubelet.StartKubeletService] = func(
	name string,
	cfg *config.Config,
) (*kubelet.StartKubeletService, error) {
	caData, err := base64.StdEncoding.DecodeString(cfg.Node.Kubelet.CACertData)
	if err != nil {
		return nil, fmt.Errorf("decode CA cert data: %w", err)
	}

	spec := kubelet.StartKubeletServiceSpec_builder{
		ControlPlane: kubelet.ControlPlane_builder{
			Server:                   ptr(cfg.Node.Kubelet.ServerURL),
			CertificateAuthorityData: caData,
		}.Build(),
		NodeAuthInfo: kubelet.NodeAuthInfo_builder{
			BootstrapTokenCredential: kubelet.KubeletBootstrapTokenCredential_builder{
				Token: ptr(cfg.Azure.BootstrapToken.Token),
			}.Build(),
		}.Build(),
		NodeLabels: maps.Clone(cfg.Node.Labels),
		KubeletConfig: kubelet.KubeletConfig_builder{
			KubeReserved:         maps.Clone(cfg.Node.Kubelet.KubeReserved),
			EvictionHard:         maps.Clone(cfg.Node.Kubelet.EvictionHard),
			Verbosity:            ptr(int32(cfg.Node.Kubelet.Verbosity)),
			ImageGcHighThreshold: ptr(int32(cfg.Node.Kubelet.ImageGCHighThreshold)),
			ImageGcLowThreshold:  ptr(int32(cfg.Node.Kubelet.ImageGCLowThreshold)),
			ClusterDns:           []string{cfg.Node.Kubelet.DNSServiceIP},
			MaxPods:              ptr(int32(cfg.Node.MaxPods)),
		}.Build(),
	}.Build()

	return kubelet.StartKubeletService_builder{
		Metadata: componentAction(name),
		Spec:     spec,
	}.Build(), nil
}
