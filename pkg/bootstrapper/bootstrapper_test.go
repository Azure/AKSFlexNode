package bootstrapper

import (
	"testing"

	"github.com/sirupsen/logrus"
	"go.goms.io/aks/AKSFlexNode/pkg/config"
)

// TestNew verifies that the Bootstrapper constructor initializes correctly.
// Test: Creates a new Bootstrapper with config and logger
// Expected: Returns non-nil Bootstrapper with initialized BaseExecutor
func TestNew(t *testing.T) {
	cfg := &config.Config{}
	logger := logrus.New()

	bootstrapper := New(cfg, logger)

	if bootstrapper == nil {
		t.Fatal("New should not return nil")
	}

	if bootstrapper.BaseExecutor == nil {
		t.Error("BaseExecutor should be initialized")
	}
}

// TestBootstrapperStructure verifies the Bootstrapper structure and initialization.
// Test: Creates a Bootstrapper and checks its structure components
// Expected: Bootstrapper and BaseExecutor should both be properly initialized
// Note: Full Bootstrap/Unbootstrap integration tests require complete system environment
func TestBootstrapperStructure(t *testing.T) {
	// Test that Bootstrapper has the expected structure
	cfg := &config.Config{}
	logger := logrus.New()
	bootstrapper := New(cfg, logger)

	// Just verify that the bootstrapper is initialized properly
	// Methods Bootstrap and Unbootstrap exist as methods on the struct
	if bootstrapper == nil {
		t.Fatal("Bootstrapper should not be nil")
	}

	if bootstrapper.BaseExecutor == nil {
		t.Error("BaseExecutor should be initialized")
	}
}

// Note: Full integration tests for Bootstrap and Unbootstrap require
// a complete system environment with Arc, containers, k8s, etc.
// Those should be in integration test suite, not unit tests.
