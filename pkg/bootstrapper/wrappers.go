package bootstrapper

import (
	"context"
	"fmt"
	"log/slog"

	"google.golang.org/grpc"

	"github.com/Azure/AKSFlexNode/components/arc"
	"github.com/Azure/AKSFlexNode/components/npd"
	"github.com/Azure/AKSFlexNode/components/services/actions"
	pkgarc "github.com/Azure/AKSFlexNode/pkg/components/arc"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/agent/phases"
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
