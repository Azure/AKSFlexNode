package yurthub

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
)

const directFixture = `apiVersion: v1
kind: Config
current-context: node
contexts:
- name: node
  context:
    cluster: cloud
    user: node
clusters:
- name: cloud
  cluster:
    server: https://api.example.test:443
    certificate-authority: /etc/kubernetes/pki/apiserver-client-ca.crt
users:
- name: node
  user:
    client-certificate: /var/lib/kubelet/pki/kubelet-client-current.pem
    client-key: /var/lib/kubelet/pki/kubelet-client-current.pem
`

func TestRoutingRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, directKubeconfig), directFixture)
	writeFixture(t, filepath.Join(dir, "etc/systemd/system/kubelet.service.d/10-customer.conf"), "customer")
	for range 2 {
		if err := configureRouting(dir); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, proxyKubeconfig))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := clientcmd.Load(data)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Clusters["cloud"].Server != proxyURL || cfg.AuthInfos["node"].ClientCertificate != "/var/lib/kubelet/pki/kubelet-client-current.pem" {
		t.Fatal("proxy config lost routing or identity")
	}
	info, err := os.Stat(filepath.Join(dir, proxyKubeconfig))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("proxy config permissions: %v, %v", info, err)
	}
	for i, wantRemoved := range []bool{true, false} {
		removed, err := restoreRouting(dir)
		if err != nil || removed != wantRemoved {
			t.Fatalf("restoreRouting #%d = %v, %v; want removed=%v", i, removed, err, wantRemoved)
		}
	}
	got, err := os.ReadFile(filepath.Join(dir, directKubeconfig))
	if err != nil || string(got) != directFixture {
		t.Fatal("direct config altered")
	}
	got, err = os.ReadFile(filepath.Join(dir, proxyKubeconfig))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("proxy config removed before direct access verification")
	}
	if _, err := os.Stat(filepath.Join(dir, routingDropIn)); !os.IsNotExist(err) {
		t.Fatal("routing override retained")
	}
	got, err = os.ReadFile(filepath.Join(dir, "etc/systemd/system/kubelet.service.d/10-customer.conf"))
	if err != nil || string(got) != "customer" {
		t.Fatal("customer override altered")
	}
}

func TestRoutingRejectsAmbiguousConfiguration(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"invalid config", "already proxied", "explicit proxy", "relative credentials", "missing context", "conflicting override", "masked override", "root escape"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			config := directFixture
			switch name {
			case "invalid config":
				config = "invalid: [ sensitive-token"
			case "already proxied":
				config = strings.ReplaceAll(config, "https://api.example.test:443", proxyURL)
			case "explicit proxy":
				config = strings.ReplaceAll(config, "server: https://api.example.test:443",
					"server: https://api.example.test:443\n    proxy-url: http://user:sensitive-token@proxy.example:3128")
			case "relative credentials":
				config = strings.ReplaceAll(config, "/var/lib/kubelet/pki/", "pki/")
			case "missing context":
				config = strings.ReplaceAll(config, "current-context: node", "current-context: missing")
			case "conflicting override":
				writeFixture(t, filepath.Join(dir, routingDropIn), "other owner")
			case "masked override":
				writeFixture(t, filepath.Join(dir, routingDropIn), "temporary")
				if err := os.Remove(filepath.Join(dir, routingDropIn)); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("/dev/null", filepath.Join(dir, routingDropIn)); err != nil {
					t.Fatal(err)
				}
			case "root escape":
				if err := os.Symlink(t.TempDir(), filepath.Join(dir, "etc")); err != nil {
					t.Fatal(err)
				}
			}
			writeFixture(t, filepath.Join(dir, directKubeconfig), config)
			err := configureRouting(dir)
			if err == nil || strings.Contains(err.Error(), "sensitive-token") {
				t.Fatalf("configureRouting error = %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, proxyKubeconfig)); !os.IsNotExist(err) {
				t.Fatal("published proxy config before validation succeeded")
			}
			if name == "explicit proxy" {
				if !strings.Contains(err.Error(), "explicit proxy") {
					t.Fatalf("proxy rejection not identified: %v", err)
				}
				if _, err := os.Lstat(filepath.Join(dir, routingDropIn)); !os.IsNotExist(err) {
					t.Fatal("published routing override for proxy-bearing config")
				}
				got, err := os.ReadFile(filepath.Join(dir, directKubeconfig))
				if err != nil || string(got) != config {
					t.Fatal("direct config altered")
				}
			}
			if name == "conflicting override" || name == "masked override" {
				if _, err := restoreRouting(dir); err == nil {
					t.Fatal("restoration removed unowned/masked override")
				}
			}
		})
	}
}
