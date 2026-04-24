package bootstrapper

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"google.golang.org/grpc"

	"github.com/Azure/AKSFlexNode/components/arc"
	"github.com/Azure/AKSFlexNode/components/npd"
	"github.com/Azure/AKSFlexNode/components/services/actions"
	pkgarc "github.com/Azure/AKSFlexNode/pkg/components/arc"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// gRPC action task wrappers
//
// These wrap the existing protobuf/gRPC component actions as phases.Task so
// they can be composed alongside the shared agent library tasks in the
// bootstrap sequence. The underlying implementations are unchanged.

// installArcTask wraps the Arc installation gRPC action as a phases.Task.
type installArcTask struct {
	conn *grpc.ClientConn
	cfg  *config.Config
}

func InstallArc(conn *grpc.ClientConn, cfg *config.Config) phases.Task {
	return &installArcTask{conn: conn, cfg: cfg}
}

func (t *installArcTask) Name() string { return "install-arc" }

func (t *installArcTask) Do(ctx context.Context) error {
	action, err := resolveInstallArc("install-arc", t.cfg)
	if err != nil {
		return fmt.Errorf("resolve install-arc action: %w", err)
	}
	_, err = actions.ApplyAction(t.conn, ctx, action)
	return err
}

func resolveInstallArc(name string, cfg *config.Config) (*arc.InstallArc, error) {
	spec := arc.InstallArcSpec_builder{
		SubscriptionId: &cfg.Azure.SubscriptionID,
		TenantId:       &cfg.Azure.TenantID,
		ResourceGroup:  ptrWithDefault(cfg.GetArcResourceGroup(), ""),
		Location:       ptrWithDefault(cfg.GetArcLocation(), ""),
		MachineName:    ptrWithDefault(cfg.GetArcMachineName(), ""),
		Tags:           cfg.GetArcTags(),
		AksClusterName: ptrWithDefault(cfg.GetTargetClusterName(), ""),
		Enabled:        ptr(cfg.IsARCEnabled()),
	}.Build()

	return arc.InstallArc_builder{
		Metadata: componentAction(name),
		Spec:     spec,
	}.Build(), nil
}

// downloadNPDTask wraps the NPD download gRPC action as a phases.Task.
type downloadNPDTask struct {
	conn *grpc.ClientConn
	cfg  *config.Config
}

func DownloadNPD(conn *grpc.ClientConn, cfg *config.Config) phases.Task {
	return &downloadNPDTask{conn: conn, cfg: cfg}
}

func (t *downloadNPDTask) Name() string { return "download-npd" }

func (t *downloadNPDTask) Do(ctx context.Context) error {
	action, err := resolveDownloadNPD("download-npd", t.cfg)
	if err != nil {
		return fmt.Errorf("resolve download-npd action: %w", err)
	}
	_, err = actions.ApplyAction(t.conn, ctx, action)
	return err
}

func resolveDownloadNPD(name string, cfg *config.Config) (*npd.DownloadNodeProblemDetector, error) {
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

// startNPDTask wraps the NPD start gRPC action as a phases.Task.
type startNPDTask struct {
	conn *grpc.ClientConn
	cfg  *config.Config
}

func StartNPD(conn *grpc.ClientConn, cfg *config.Config) phases.Task {
	return &startNPDTask{conn: conn, cfg: cfg}
}

func (t *startNPDTask) Name() string { return "start-npd" }

func (t *startNPDTask) Do(ctx context.Context) error {
	action, err := resolveStartNPD("start-npd", t.cfg)
	if err != nil {
		return fmt.Errorf("resolve start-npd action: %w", err)
	}
	_, err = actions.ApplyAction(t.conn, ctx, action)
	return err
}

func resolveStartNPD(name string, cfg *config.Config) (*npd.StartNodeProblemDetector, error) {
	spec := npd.StartNodeProblemDetectorSpec_builder{
		ApiServer:      ptr(cfg.Node.Kubelet.ServerURL),
		KubeConfigPath: ptr(config.KubeletKubeconfigPath),
	}.Build()

	return npd.StartNodeProblemDetector_builder{
		Metadata: componentAction(name),
		Spec:     spec,
	}.Build(), nil
}

// enrichClusterConfigTask wraps the cluster config enricher as a phases.Task.
type enrichClusterConfigTask struct {
	cfg    *config.Config
	logger *slog.Logger
}

func EnrichClusterConfig(cfg *config.Config, logger *slog.Logger) phases.Task {
	return &enrichClusterConfigTask{cfg: cfg, logger: logger}
}

func (t *enrichClusterConfigTask) Name() string { return "enrich-cluster-config" }

func (t *enrichClusterConfigTask) Do(ctx context.Context) error {
	// Delegate to the existing enricher logic. We pass the config directly
	// rather than using config.GetConfig() singleton so callers control the
	// Config instance.
	enricher := newClusterConfigEnricher(t.cfg, toLogrus(t.logger))
	if enricher.IsCompleted(ctx) {
		return nil
	}
	return enricher.Execute(ctx)
}

// uninstallArcTask wraps the Arc uninstaller as a phases.Task.
type uninstallArcTask struct {
	logger *slog.Logger
}

func UninstallArc(logger *slog.Logger) phases.Task {
	return &uninstallArcTask{logger: logger}
}

func (t *uninstallArcTask) Name() string { return "uninstall-arc" }

func (t *uninstallArcTask) Do(ctx context.Context) error {
	u := pkgarc.NewUnInstaller(toLogrus(t.logger))
	return u.Execute(ctx)
}

// writeCNIConfigTask writes a default bridge CNI conflist into the nspawn
// rootfs. The unbounded library installs CNI binaries but does not write a
// conflist; without one kubelet reports NetworkNotReady and pods cannot be
// scheduled. This uses the same 10.244.0.0/16 bridge configuration that
// the previous FlexNode CNI component wrote.

//go:embed assets/99-bridge.conf
var defaultBridgeCNIConfig []byte

type writeCNIConfigTask struct {
	machineDir string
}

// WriteCNIConfig returns a task that writes the default bridge CNI config
// into the nspawn rootfs at /etc/cni/net.d/99-bridge.conf.
func WriteCNIConfig(machineDir string) phases.Task {
	return &writeCNIConfigTask{machineDir: machineDir}
}

func (t *writeCNIConfigTask) Name() string { return "write-cni-config" }

func (t *writeCNIConfigTask) Do(_ context.Context) error {
	confDir := filepath.Join(t.machineDir, "etc", "cni", "net.d")
	if err := os.MkdirAll(confDir, 0o750); err != nil { //nolint:gosec // directory needs to be traversable
		return fmt.Errorf("create CNI config directory: %w", err)
	}

	confPath := filepath.Join(confDir, "99-bridge.conf")

	// Idempotent: skip if file already exists with expected content.
	current, err := os.ReadFile(confPath) //nolint:gosec // path is constructed, not user input
	if err == nil && string(current) == string(defaultBridgeCNIConfig) {
		return nil
	}

	if err := os.WriteFile(confPath, defaultBridgeCNIConfig, 0o644); err != nil { //nolint:gosec // CNI config must be world-readable
		return fmt.Errorf("write CNI bridge config: %w", err)
	}
	return nil
}

// rootfs so that exec credential plugins can invoke it from inside the
// container. This is required for MSI and service principal auth where
// kubelet uses `aks-flex-node token kubelogin` as an exec credential plugin.
type installBinaryTask struct {
	machineDir string
}

// InstallBinary returns a task that copies the current process binary into
// the nspawn rootfs at /usr/local/bin/aks-flex-node.
func InstallBinary(machineDir string) phases.Task {
	return &installBinaryTask{machineDir: machineDir}
}

func (t *installBinaryTask) Name() string { return "install-binary-in-rootfs" }

func (t *installBinaryTask) Do(_ context.Context) error {
	selfPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve self executable: %w", err)
	}

	destPath := filepath.Join(t.machineDir, "usr", "local", "bin", "aks-flex-node")
	if err := os.MkdirAll(filepath.Dir(destPath), 0o750); err != nil { //nolint:gosec // directory needs to be traversable
		return fmt.Errorf("create destination directory: %w", err)
	}

	src, err := os.Open(selfPath) //nolint:gosec // path is from os.Executable(), not user input
	if err != nil {
		return fmt.Errorf("open self binary: %w", err)
	}
	defer func() { _ = src.Close() }()

	dst, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o750) //nolint:gosec // binary must be executable
	if err != nil {
		return fmt.Errorf("create destination binary: %w", err)
	}
	defer func() { _ = dst.Close() }()

	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("copy binary: %w", err)
	}

	return nil
}
