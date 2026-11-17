package runtimeprovider

import (
	"testing"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

var newNoneRuntime = NewNoneRuntime

func mustNodeProvider(t *testing.T, mode isolation.Mode) NodeProvider {
	t.Helper()
	provider, err := New(mode, HostServices{})
	if err != nil {
		t.Fatal(err)
	}
	return provider
}
