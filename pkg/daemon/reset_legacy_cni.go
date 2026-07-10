package daemon

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/Azure/AKSFlexNode/pkg/utils/utilexec"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

const (
	legacyCNIBridgeName       = "cni0"
	legacyCNINetworkName      = "bridge"
	legacyCNINFTablesTable    = "cni_plugins_masquerade"
	legacyCNINFTablesChain    = "masq_checks"
	legacyCNIPostroutingChain = "POSTROUTING"
)

type cleanupLegacyCNI struct {
	log      *slog.Logger
	executor utilexec.Interface
}

// cleanupLegacyBridgeCNI removes state created by the retired 99-bridge.conf
// configuration.
// TODO: Remove this compatibility cleanup in v0.1.5.
func cleanupLegacyBridgeCNI(log *slog.Logger) phases.Task {
	return &cleanupLegacyCNI{
		log:      log,
		executor: utilexec.New(),
	}
}

func (t *cleanupLegacyCNI) Name() string { return "cleanup-legacy-cni" }

func (t *cleanupLegacyCNI) Do(ctx context.Context) error {
	t.removeBridge(ctx)
	t.removeIPTablesRules(ctx)
	t.removeNFTablesRules(ctx)
	return nil
}

func (t *cleanupLegacyCNI) output(ctx context.Context, name string, args ...string) (string, error) {
	cmd := t.executor.CommandContext(ctx, name, args...)
	output, err := cmd.CombinedOutput()
	trimmed := strings.TrimRight(string(output), "\r\n")
	if trimmed != "" {
		t.log.Debug("legacy CNI cleanup command output", "command", name, "output", trimmed)
	}
	if err != nil {
		return "", fmt.Errorf("run %s: %w", name, err)
	}
	return trimmed, nil
}

func (t *cleanupLegacyCNI) run(ctx context.Context, name string, args ...string) error {
	_, err := t.output(ctx, name, args...)
	return err
}

func (t *cleanupLegacyCNI) removeBridge(ctx context.Context) {
	if _, err := t.output(ctx, "ip", "link", "show", "dev", legacyCNIBridgeName); err != nil {
		t.log.Debug("legacy CNI bridge is already absent", "interface", legacyCNIBridgeName)
		return
	}

	t.log.Info("removing legacy CNI bridge", "interface", legacyCNIBridgeName)
	if err := t.run(ctx, "ip", "link", "delete", legacyCNIBridgeName); err != nil {
		t.log.Warn("failed to remove legacy CNI bridge", "interface", legacyCNIBridgeName, "error", err)
	}
}

func (t *cleanupLegacyCNI) removeIPTablesRules(ctx context.Context) {
	rules, err := t.output(ctx, "iptables-save", "-t", "nat")
	if err != nil {
		t.log.Debug("legacy CNI iptables rules are unavailable", "error", err)
		return
	}

	chains := legacyCNIIPTablesChains(rules)
	if len(chains) == 0 {
		return
	}

	listing, err := t.output(ctx, "iptables", "--wait", "-t", "nat", "-L", legacyCNIPostroutingChain, "--line-numbers", "-n")
	if err != nil {
		t.log.Warn("failed to list legacy CNI iptables references", "error", err)
		return
	}

	for _, lineNumber := range legacyCNIIPTablesRuleNumbers(listing, chains) {
		if err := t.run(ctx, "iptables", "--wait", "-t", "nat", "-D", legacyCNIPostroutingChain, strconv.Itoa(lineNumber)); err != nil {
			t.log.Warn("failed to delete legacy CNI iptables reference", "lineNumber", lineNumber, "error", err)
		}
	}
	for _, chain := range chains {
		t.log.Info("removing legacy CNI iptables chain", "chain", chain)
		if err := t.run(ctx, "iptables", "--wait", "-t", "nat", "-F", chain); err != nil {
			t.log.Warn("failed to flush legacy CNI iptables chain", "chain", chain, "error", err)
			continue
		}
		if err := t.run(ctx, "iptables", "--wait", "-t", "nat", "-X", chain); err != nil {
			t.log.Warn("failed to delete legacy CNI iptables chain", "chain", chain, "error", err)
		}
	}
}

func (t *cleanupLegacyCNI) removeNFTablesRules(ctx context.Context) {
	args := []string{"-a", "list", "chain", "inet", legacyCNINFTablesTable, legacyCNINFTablesChain}
	rules, err := t.output(ctx, "nft", args...)
	if err != nil {
		t.log.Debug("legacy CNI nftables rules are unavailable", "error", err)
		return
	}

	for _, handle := range legacyCNINFTablesHandles(rules) {
		t.log.Info("removing legacy CNI nftables rule", "handle", handle)
		if err := t.run(ctx, "nft", "delete", "rule", "inet", legacyCNINFTablesTable, legacyCNINFTablesChain, "handle", handle); err != nil {
			t.log.Warn("failed to delete legacy CNI nftables rule", "handle", handle, "error", err)
		}
	}
}

func legacyCNIIPTablesChains(rules string) []string {
	chains := map[string]struct{}{}
	for scanner := bufio.NewScanner(strings.NewReader(rules)); scanner.Scan(); {
		line := scanner.Text()
		if !isLegacyCNIRule(line) {
			continue
		}
		fields := strings.Fields(line)
		for i, field := range fields {
			if (field != "-A" && field != "-j") || i+1 >= len(fields) {
				continue
			}
			chain := strings.Trim(fields[i+1], `"'`)
			if strings.HasPrefix(chain, "CNI-") {
				chains[chain] = struct{}{}
			}
		}
	}

	result := make([]string, 0, len(chains))
	for chain := range chains {
		result = append(result, chain)
	}
	sort.Strings(result)
	return result
}

func isLegacyCNIRule(rule string) bool {
	return strings.Contains(rule, fmt.Sprintf(`name: \"%s\"`, legacyCNINetworkName)) ||
		strings.Contains(rule, fmt.Sprintf(`name: "%s"`, legacyCNINetworkName))
}

func legacyCNIIPTablesRuleNumbers(listing string, chains []string) []int {
	chainSet := make(map[string]struct{}, len(chains))
	for _, chain := range chains {
		chainSet[chain] = struct{}{}
	}

	var result []int
	for scanner := bufio.NewScanner(strings.NewReader(listing)); scanner.Scan(); {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		if _, ok := chainSet[fields[1]]; !ok {
			continue
		}
		lineNumber, err := strconv.Atoi(fields[0])
		if err == nil {
			result = append(result, lineNumber)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(result)))
	return result
}

func legacyCNINFTablesHandles(rules string) []string {
	var result []string
	for scanner := bufio.NewScanner(strings.NewReader(rules)); scanner.Scan(); {
		line := scanner.Text()
		if !strings.Contains(line, "net: "+legacyCNINetworkName+",") {
			continue
		}
		fields := strings.Fields(line)
		for i := 0; i+1 < len(fields); i++ {
			if fields[i] == "handle" {
				if _, err := strconv.ParseUint(fields[i+1], 10, 64); err == nil {
					result = append(result, fields[i+1])
				}
			}
		}
	}
	return result
}
