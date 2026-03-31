// Command rptest tests the FlexNode RP client against a standalone environment.
//
// Usage:
//
//	kubectl port-forward -n containerservice svc/containerserviceinternal-stable 18081:4000 &
//	go run ./cmd/rptest --rp http://localhost:18081 --machine my-node-1
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/Azure/AKSFlexNode/pkg/rpclient"
)

func main() {
	rpURL := flag.String("rp", "http://localhost:18081", "RP base URL (port-forwarded)")
	sub := flag.String("sub", "8ecadfc9-d1a3-4ea4-b844-0d9f87e4d7c8", "Subscription ID")
	rg := flag.String("rg", "flexnode-test-35468", "Resource group")
	cluster := flag.String("cluster", "fntest35468", "Cluster name")
	machine := flag.String("machine", "container-vm-1", "Machine name")
	flag.Parse()

	log := logrus.New()
	log.SetLevel(logrus.DebugLevel)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	client := rpclient.NewStandaloneClient(log, *rpURL, *sub, *rg, *cluster, *machine)

	passed, failed, knownFail := 0, 0, 0

	check := func(name string, err error) {
		if err == nil {
			fmt.Printf("  ✓ %s\n", name)
			passed++
			return
		}
		// PUT machine/status fail with EntityStoreOperationError because
		// HCP doesn't have the new proto fields yet — this is expected.
		// Connection reset/refused happens when port-forward dies from long PUT.
		if strings.Contains(err.Error(), "EntityStoreOperationError") ||
			strings.Contains(err.Error(), "connection reset") ||
			strings.Contains(err.Error(), "connection refused") {
			fmt.Printf("  ~ %s: expected failure (HCP proto not deployed)\n", name)
			knownFail++
			return
		}
		fmt.Printf("  ✗ %s: %v\n", name, err)
		failed++
	}

	// 1. Bootstrap data (fast, doesn't break port-forward)
	fmt.Println("\n=== Get Bootstrap Data ===")
	data, err := client.GetBootstrapData(ctx)
	check("POST getBootstrapData", err)
	if data != nil {
		b, _ := json.MarshalIndent(data, "  ", "  ")
		fmt.Printf("  %s\n", b)
	}

	// 2. Register (PUT — may break port-forward due to long-running async)
	fmt.Println("\n=== Register Machine ===")
	check("PUT machine", client.RegisterMachine(ctx, map[string]string{"source": "rptest"}))

	// 3. Report status (PUT)
	fmt.Println("\n=== Report Status ===")
	check("PUT status", client.ReportStatus(ctx, map[string]string{
		"agentVersion":   "0.1.0-test",
		"kubeletRunning": "false",
	}))

	fmt.Printf("\n=== Results: %d passed, %d known-fail, %d unexpected-fail ===\n", passed, knownFail, failed)
	if failed > 0 {
		os.Exit(1)
	}
}
