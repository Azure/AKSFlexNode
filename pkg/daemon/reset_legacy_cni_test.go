package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

func TestLegacyCNIIPTablesChains(t *testing.T) {
	t.Parallel()

	rules := `*nat
:CNI-111111111111111111111111 - [0:0]
:CNI-222222222222222222222222 - [0:0]
-A POSTROUTING -s 10.244.0.2/32 -m comment --comment "name: \"bridge\" id: \"pod-a\"" -j CNI-111111111111111111111111
-A CNI-111111111111111111111111 -d 10.244.0.2/24 -m comment --comment "name: \"bridge\" id: \"pod-a\"" -j ACCEPT
-A CNI-111111111111111111111111 ! -d 224.0.0.0/4 -m comment --comment "name: \"bridge\" id: \"pod-a\"" -j MASQUERADE
-A POSTROUTING -s 10.240.0.2/32 -m comment --comment "name: \"unbounded\" id: \"pod-b\"" -j CNI-222222222222222222222222
COMMIT`

	want := []string{"CNI-111111111111111111111111"}
	if got := legacyCNIIPTablesChains(rules); !reflect.DeepEqual(got, want) {
		t.Fatalf("legacyCNIIPTablesChains() = %v, want %v", got, want)
	}
}

func TestLegacyCNIIPTablesRuleNumbers(t *testing.T) {
	t.Parallel()

	listing := `Chain POSTROUTING (policy ACCEPT)
num  target                         prot opt source               destination
1    CNI-111111111111111111111111  all  --  10.244.0.2           0.0.0.0/0
2    MASQUERADE                     all  --  10.240.0.0/16        0.0.0.0/0
4    CNI-333333333333333333333333  all  --  10.244.0.3           0.0.0.0/0
7    CNI-111111111111111111111111  all  --  10.244.0.4           0.0.0.0/0`

	chains := []string{"CNI-111111111111111111111111", "CNI-333333333333333333333333"}
	want := []int{7, 4, 1}
	if got := legacyCNIIPTablesRuleNumbers(listing, chains); !reflect.DeepEqual(got, want) {
		t.Fatalf("legacyCNIIPTablesRuleNumbers() = %v, want %v", got, want)
	}
}

func TestLegacyCNINFTablesHandles(t *testing.T) {
	t.Parallel()

	rules := `table inet cni_plugins_masquerade {
  chain masq_checks {
    ip saddr == 10.244.0.2 ip daddr != 10.244.0.0/16 masquerade comment "abc, net: bridge, if: eth0, id: pod-a" # handle 17
    ip saddr == 10.240.0.2 ip daddr != 10.240.0.0/16 masquerade comment "def, net: unbounded, if: eth0, id: pod-b" # handle 21
    ip saddr == 10.244.0.3 ip daddr != 10.244.0.0/16 masquerade comment "ghi, net: bridge, if: eth0, id: pod-c" # handle 23
  }
}`

	want := []string{"17", "23"}
	if got := legacyCNINFTablesHandles(rules); !reflect.DeepEqual(got, want) {
		t.Fatalf("legacyCNINFTablesHandles() = %v, want %v", got, want)
	}
}

func TestCleanupLegacyBridgeCNIDo(t *testing.T) {
	t.Parallel()

	executor := &fakeLegacyCNIExecutor{outputs: map[string]fakeLegacyCNIOutput{
		"ip link show dev cni0": {output: "9: cni0: <BROADCAST,MULTICAST,UP>"},
		"iptables-save -t nat": {output: `-A POSTROUTING -s 10.244.0.2/32 -m comment --comment "name: \"bridge\" id: \"pod-a\"" -j CNI-111111111111111111111111
-A CNI-111111111111111111111111 -d 10.244.0.0/16 -m comment --comment "name: \"bridge\" id: \"pod-a\"" -j ACCEPT`},
		"iptables --wait -t nat -L POSTROUTING --line-numbers -n": {output: `1 CNI-111111111111111111111111 all -- 10.244.0.2 0.0.0.0/0
4 CNI-111111111111111111111111 all -- 10.244.0.3 0.0.0.0/0`},
		"nft -a list chain inet cni_plugins_masquerade masq_checks": {output: `ip saddr == 10.244.0.2 masquerade comment "abc, net: bridge, if: eth0, id: pod-a" # handle 17`},
	}}
	task := &cleanupLegacyCNI{log: slog.Default(), executor: executor}

	if err := task.Do(t.Context()); err != nil {
		t.Fatalf("Do() error = %v", err)
	}

	want := []string{
		"ip link show dev cni0",
		"ip link delete cni0",
		"iptables-save -t nat",
		"iptables --wait -t nat -L POSTROUTING --line-numbers -n",
		"iptables --wait -t nat -D POSTROUTING 4",
		"iptables --wait -t nat -D POSTROUTING 1",
		"iptables --wait -t nat -F CNI-111111111111111111111111",
		"iptables --wait -t nat -X CNI-111111111111111111111111",
		"nft -a list chain inet cni_plugins_masquerade masq_checks",
		"nft delete rule inet cni_plugins_masquerade masq_checks handle 17",
	}
	if !reflect.DeepEqual(executor.commands, want) {
		t.Fatalf("commands = %v, want %v", executor.commands, want)
	}
}

func TestCleanupLegacyBridgeCNIIsIdempotentWhenStateIsAbsent(t *testing.T) {
	t.Parallel()

	executor := &fakeLegacyCNIExecutor{outputs: map[string]fakeLegacyCNIOutput{
		"ip link show dev cni0": {err: errors.New("link not found")},
		"iptables-save -t nat":  {output: "*nat\nCOMMIT"},
		"nft -a list chain inet cni_plugins_masquerade masq_checks": {err: errors.New("table not found")},
	}}
	task := &cleanupLegacyCNI{log: slog.Default(), executor: executor}

	if err := task.Do(t.Context()); err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	want := []string{
		"ip link show dev cni0",
		"iptables-save -t nat",
		"nft -a list chain inet cni_plugins_masquerade masq_checks",
	}
	if !reflect.DeepEqual(executor.commands, want) {
		t.Fatalf("commands = %v, want %v", executor.commands, want)
	}
}

type fakeLegacyCNIOutput struct {
	output string
	err    error
}

type fakeLegacyCNIExecutor struct {
	outputs  map[string]fakeLegacyCNIOutput
	commands []string
}

func (f *fakeLegacyCNIExecutor) Command(name string, args ...string) *exec.Cmd {
	return f.command(context.Background(), name, args...)
}

func (f *fakeLegacyCNIExecutor) CommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	return f.command(ctx, name, args...)
}

func (f *fakeLegacyCNIExecutor) command(ctx context.Context, name string, args ...string) *exec.Cmd {
	command := strings.Join(append([]string{name}, args...), " ")
	f.commands = append(f.commands, command)
	output := f.outputs[command]

	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestLegacyCNICommandProcess", "--")
	cmd.Env = append(os.Environ(),
		"GO_WANT_LEGACY_CNI_COMMAND_PROCESS=1",
		"LEGACY_CNI_COMMAND_OUTPUT="+output.output,
	)
	if output.err != nil {
		cmd.Env = append(cmd.Env, "LEGACY_CNI_COMMAND_FAIL=1")
	}
	return cmd
}

func TestLegacyCNICommandProcess(t *testing.T) {
	if os.Getenv("GO_WANT_LEGACY_CNI_COMMAND_PROCESS") != "1" {
		return
	}
	_, _ = fmt.Fprint(os.Stdout, os.Getenv("LEGACY_CNI_COMMAND_OUTPUT"))
	if os.Getenv("LEGACY_CNI_COMMAND_FAIL") == "1" {
		os.Exit(1)
	}
	os.Exit(0)
}
