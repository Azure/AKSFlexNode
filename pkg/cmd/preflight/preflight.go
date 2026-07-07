package preflight

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/logger"
	"github.com/Azure/AKSFlexNode/pkg/npd"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases/host"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

type handler struct {
	configPath            string
	ignorePreflightErrors []string
	failOnWarnings        bool
	output                string
	writer                io.Writer
}

// NewCommand returns the preflight command.
func NewCommand() *cobra.Command {
	h := &handler{writer: os.Stdout}

	cmd := &cobra.Command{
		Use:   "preflight",
		Short: "Run non-mutating preflight checks",
		Long:  "Run non-mutating preflight checks for the host and AKS Flex Node configuration before node bootstrap.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return h.execute(cmd.Context())
		},
	}

	cmd.Flags().StringVar(&h.configPath, "config", "", "Path to configuration JSON file (required)")
	_ = cmd.MarkFlagRequired("config")
	cmd.Flags().StringSliceVar(
		&h.ignorePreflightErrors,
		"ignore-preflight-errors",
		nil,
		"Comma-separated preflight check names whose errors should be reported as warnings",
	)
	cmd.Flags().BoolVar(&h.failOnWarnings, "fail-on-warnings", false, "Fail when any preflight warning is returned")
	cmd.Flags().StringVar(&h.output, "output", "text", "Output format: text or json")

	return cmd
}

func (h *handler) execute(ctx context.Context) error {
	output, err := normalizeOutput(h.output)
	if err != nil {
		return err
	}

	cfg, err := config.LoadConfig(h.configPath)
	if err != nil {
		return fmt.Errorf("failed to load config from %s: %w", h.configPath, err)
	}
	log := createPreflightLogger(cfg.Agent.LogLevel)

	agentCfg, gs, err := config.ResolveMachineGoalState(log, cfg, goalstates.NSpawnMachineKube1)
	if err != nil {
		return fmt.Errorf("preflight failed to resolve goal state: %w", err)
	}

	checks := preflight.Flatten(
		host.Preflight(log, *agentCfg, gs),
		nodestart.Preflight(log, *agentCfg, gs),
		rootfs.Preflight(log, *agentCfg, gs),
		npd.Preflight(cfg),
	)

	report := preflight.Run(ctx, checks, preflight.Options{
		IgnoreErrors:   h.ignorePreflightErrors,
		FailOnWarnings: h.failOnWarnings,
	})

	switch output {
	case "text":
		if err := writeText(h.writer, report); err != nil {
			return err
		}
	case "json":
		enc := json.NewEncoder(h.writer)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return err
		}
	}

	return report.Err(h.failOnWarnings)
}

func normalizeOutput(output string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(output)) {
	case "", "text":
		return "text", nil
	case "json":
		return "json", nil
	default:
		return "", fmt.Errorf("unsupported output format %q", output)
	}
}

// createPreflightLogger keeps diagnostics on stderr so text and JSON reports on
// stdout remain machine-readable, and avoids creating log files during preflight.
func createPreflightLogger(level string) *slog.Logger {
	logLevel, err := logger.ParseLogLevel(level)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Warning: %v. Using 'info' level as default.\n", err)
		logLevel = slog.LevelInfo
	}

	levelVar := &slog.LevelVar{}
	levelVar.Set(logLevel)

	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: levelVar}))
}

func writeText(w io.Writer, report preflight.Report) error {
	if _, err := fmt.Fprintln(w, "[preflight] Running AKS Flex Node preflight checks"); err != nil {
		return err
	}

	var errors []preflight.Result
	for _, result := range report.Checks {
		switch result.Severity {
		case preflight.SeverityOK:
			if err := writeResult(w, "OK", result); err != nil {
				return err
			}
		case preflight.SeverityError:
			errors = append(errors, result)
		case preflight.SeverityWarning:
			if err := writeResult(w, "WARNING", result); err != nil {
				return err
			}
		}
	}

	if len(errors) == 0 {
		return nil
	}

	if _, err := fmt.Fprintln(w, "[preflight] Some fatal errors occurred:"); err != nil {
		return err
	}
	for _, result := range errors {
		if err := writeResult(w, "ERROR", result); err != nil {
			return err
		}
	}

	_, err := fmt.Fprintln(w, "[preflight] If you know what you are doing, you can make a check non-fatal with `--ignore-preflight-errors=...`")
	return err
}

func writeResult(w io.Writer, status string, result preflight.Result) error {
	if _, err := fmt.Fprintf(w, "\t[%s %s]: %s", status, result.Name, result.Message); err != nil {
		return err
	}
	if result.Target != "" {
		if _, err := fmt.Fprintf(w, " (target: %s)", result.Target); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w)
	return err
}
