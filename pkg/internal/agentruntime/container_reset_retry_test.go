package agentruntime

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContainerTeardownRetainsOwnershipForCLIReselection(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		stopFailure bool
		snapshot    bool
	}{
		{
			name:        "stop failure without snapshot",
			stopFailure: true,
		},
		{
			name:        "stop failure with snapshot",
			stopFailure: true,
			snapshot:    true,
		},
		{
			name: "config failure without snapshot",
		},
		{
			name:     "config failure with snapshot",
			snapshot: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newContainerTeardownFixture(t)
			if err := os.WriteFile(f.unit, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if !tc.snapshot {
				if err := os.RemoveAll(f.snapshot); err != nil {
					t.Fatal(err)
				}
			}
			if tc.stopFailure {
				// SDK Destroy deletes the stopped slot even when stop returned an error.
				f.stopErr = errors.New("stop timed out after the container exited")
			} else {
				// A nonempty directory makes RemoveConfig fail even when tests run as root.
				if err := os.Remove(f.modules); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(f.modules, 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(f.modules, "blocked"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}

			err := containerTeardown(f.teardown).Do(t.Context())
			if tc.stopFailure {
				if !errors.Is(err, f.stopErr) {
					t.Fatalf("stop error lost: %v", err)
				}
				if _, err := os.Stat(f.sharedConfig); err != nil {
					t.Fatalf("failed destroy removed shared config: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "remove OCI worker configuration") {
				t.Fatalf("expected RemoveConfig error: %v", err)
			}
			for _, path := range []string{f.unit, f.teardown.Layout.RootfsDir("kube1")} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("expected slot trace removed at %s: %v", path, err)
				}
			}
			if tc.snapshot {
				if _, err := os.Stat(filepath.Join(f.snapshot, "credentials")); err != nil {
					t.Fatalf("failed teardown removed snapshot: %v", err)
				}
			}

			f.stopErr = nil
			if !tc.stopFailure {
				if err := os.RemoveAll(f.modules); err != nil {
					t.Fatal(err)
				}
			}
			// There is no applied State. A fresh CLI uses this exact SDK detector
			// before selecting full OCI cleanup rather than the orphan-snapshot path.
			retry := *f.teardown
			if !retry.Present() {
				t.Fatal("no-State CLI retry would no longer select OCI")
			}
			for range 2 {
				if err := containerTeardown(&retry).Do(t.Context()); err != nil {
					t.Fatalf("fresh OCI cleanup retry: %v", err)
				}
			}
			if retry.Present() {
				t.Fatal("successful teardown retained OCI ownership traces")
			}
			for _, path := range []string{f.sharedConfig, f.modules, f.snapshot} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("retry retained %s: %v", path, err)
				}
			}
			if _, err := os.Stat(f.unrelated); err != nil {
				t.Fatalf("retry removed unrelated SDK data: %v", err)
			}
		})
	}
}
