package drift

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubectl/pkg/drain"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/kube"
	"github.com/Azure/AKSFlexNode/pkg/utils"
)

const defaultDrainTimeout = 10 * time.Minute

type nodeMaintenance interface {
	IsCordoned(ctx context.Context, nodeName string) (bool, error)
	Cordon(ctx context.Context, nodeName string) error
	Drain(ctx context.Context, nodeName string) error
	Uncordon(ctx context.Context, nodeName string) error
}

type kubeNodeMaintenance struct {
	cfg    *config.Config
	logger *logrus.Logger

	mu       sync.Mutex
	client   *kubernetes.Clientset
	initFrom string
}

func newKubeNodeMaintenance(cfg *config.Config, logger *logrus.Logger) *kubeNodeMaintenance {
	if logger == nil {
		logger = logrus.New()
	}
	return &kubeNodeMaintenance{cfg: cfg, logger: logger}
}

func (m *kubeNodeMaintenance) IsCordoned(ctx context.Context, nodeName string) (bool, error) {
	cs, err := m.clientset(ctx)
	if err != nil {
		return false, err
	}
	n, err := cs.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	return n.Spec.Unschedulable, nil
}

func (m *kubeNodeMaintenance) Cordon(ctx context.Context, nodeName string) error {
	return m.cordonOrUncordon(ctx, nodeName, true)
}

func (m *kubeNodeMaintenance) Uncordon(ctx context.Context, nodeName string) error {
	return m.cordonOrUncordon(ctx, nodeName, false)
}

func (m *kubeNodeMaintenance) Drain(ctx context.Context, nodeName string) error {
	cs, err := m.clientset(ctx)
	if err != nil {
		return err
	}

	h := m.drainHelper(ctx, cs)
	if err := drain.RunNodeDrain(h, nodeName); err != nil {
		if shouldRetryWithAdmin(err) {
			cs2, adminErr := m.forceAdminClientset(ctx)
			if adminErr == nil {
				h2 := m.drainHelper(ctx, cs2)
				return drain.RunNodeDrain(h2, nodeName)
			}
			// Log failure to obtain admin clientset before returning the original error.
			m.logger.WithError(adminErr).WithField("node", nodeName).Warn(
				"failed to get admin clientset for drain retry; returning original error",
			)
		}
		return err
	}
	return nil
}

func (m *kubeNodeMaintenance) cordonOrUncordon(ctx context.Context, nodeName string, cordon bool) error {
	cs, err := m.clientset(ctx)
	if err != nil {
		return err
	}

	n, err := cs.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	h := m.drainHelper(ctx, cs)
	if err := drain.RunCordonOrUncordon(h, n, cordon); err != nil {
		if shouldRetryWithAdmin(err) {
			cs2, adminErr := m.forceAdminClientset(ctx)
			if adminErr == nil {
				n2, err2 := cs2.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
				if err2 != nil {
					return err2
				}
				h2 := m.drainHelper(ctx, cs2)
				err2 = drain.RunCordonOrUncordon(h2, n2, cordon)
				return err2
			}
			// Log failure to obtain admin clientset before returning the original error.
			m.logger.WithError(adminErr).WithField("node", nodeName).Warn(
				"failed to get admin clientset for cordon/uncordon retry; returning original error",
			)
		}
		return err
	}
	return nil
}

func (m *kubeNodeMaintenance) drainHelper(ctx context.Context, cs *kubernetes.Clientset) *drain.Helper {
	out := io.Discard
	errOut := io.Discard
	if m.logger != nil && m.logger.IsLevelEnabled(logrus.DebugLevel) {
		w := &logrusLineWriter{logger: m.logger, level: logrus.DebugLevel}
		out = w
		errOut = w
	}

	return &drain.Helper{
		Ctx:                 ctx,
		Client:              cs,
		Force:               false,
		GracePeriodSeconds:  -1,
		IgnoreAllDaemonSets: true,
		DeleteEmptyDirData:  false,
		Timeout:             defaultDrainTimeout,
		Out:                 out,
		ErrOut:              errOut,
	}
}

func (m *kubeNodeMaintenance) clientset(ctx context.Context) (*kubernetes.Clientset, error) {
	m.mu.Lock()
	cs := m.client
	m.mu.Unlock()
	if cs != nil {
		return cs, nil
	}

	// Prefer an admin client for maintenance operations (cordon/drain) because
	// the kubelet/node identity is subject to NodeRestriction and may be unable
	// to evict or even read pods once they are being deleted.
	if m.cfg != nil {
		cs, err := m.forceAdminClientset(ctx)
		if err == nil {
			return cs, nil
		}
		if m.logger != nil {
			m.logger.WithError(err).Debug("Failed to create admin clientset for node maintenance; falling back to kubelet kubeconfig")
		}
	}

	// Fall back to the local kubelet kubeconfig if present.
	if utils.FileExists(config.KubeletKubeconfigPath) {
		cs, err := kube.KubeletClientset()
		if err == nil {
			m.mu.Lock()
			m.client = cs
			m.initFrom = "kubelet-kubeconfig"
			m.mu.Unlock()
			return cs, nil
		}
		if m.logger != nil {
			m.logger.WithError(err).Debug("Failed to create kubelet clientset for node maintenance")
		}
	}

	// Last resort: admin kubeconfig via the AKS management plane (may still fail if cfg is nil).
	return m.forceAdminClientset(ctx)
}

func (m *kubeNodeMaintenance) forceAdminClientset(ctx context.Context) (*kubernetes.Clientset, error) {
	if m.cfg == nil {
		return nil, errors.New("config is required to fetch cluster admin kubeconfig")
	}

	cs, err := kube.AdminClientset(ctx, m.cfg)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.client = cs
	m.initFrom = "aks-admin-kubeconfig"
	m.mu.Unlock()

	return cs, nil
}

func shouldRetryWithAdmin(err error) bool {
	if err == nil {
		return false
	}
	// Prefer structured detection when possible...
	if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
		return true
	}
	// ...but kubectl drain frequently wraps StatusErrors into plain strings.
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "forbidden") || strings.Contains(msg, "unauthorized") {
		return true
	}
	// Admin kubeconfigs/certs can expire; transport-layer TLS failures won't be classified
	// as apierrors.*. Treat these as signals to refresh credentials and retry.
	if strings.Contains(msg, "x509:") || strings.Contains(msg, "tls:") {
		return true
	}
	return false
}

type cordonDrainState struct {
	mu          sync.Mutex
	uncordon    bool
	nodeName    string
	initialized bool
}

func (s *cordonDrainState) set(nodeName string, uncordon bool) {
	s.mu.Lock()
	s.nodeName = nodeName
	s.uncordon = uncordon
	s.initialized = true
	s.mu.Unlock()
}

func (s *cordonDrainState) shouldUncordon(nodeName string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.initialized && s.nodeName == nodeName && s.uncordon
}

type cordonAndDrainExecutor struct {
	name   string
	logger *logrus.Logger
	ops    nodeMaintenance
	state  *cordonDrainState
}

func newCordonAndDrainExecutor(name string, logger *logrus.Logger, ops nodeMaintenance, state *cordonDrainState) *cordonAndDrainExecutor {
	return &cordonAndDrainExecutor{name: name, logger: logger, ops: ops, state: state}
}

func (e *cordonAndDrainExecutor) GetName() string { return e.name }

func (e *cordonAndDrainExecutor) IsCompleted(context.Context) bool { return false }

func (e *cordonAndDrainExecutor) Execute(ctx context.Context) error {
	if e.ops == nil {
		return errors.New("node maintenance is nil")
	}

	nodeName, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("failed to get hostname for node maintenance: %w", err)
	}

	alreadyCordoned, err := e.ops.IsCordoned(ctx, nodeName)
	if err != nil {
		return fmt.Errorf("failed to check if node is cordoned: %w", err)
	}

	// Only uncordon if we changed the scheduling state.
	uncordonAfter := !alreadyCordoned
	cordonedByUs := false

	if !alreadyCordoned {
		if e.logger != nil {
			e.logger.Infof("Cordoning node %s before kubelet upgrade", nodeName)
		}
		if err := e.ops.Cordon(ctx, nodeName); err != nil {
			return fmt.Errorf("failed to cordon node %s: %w", nodeName, err)
		}
		cordonedByUs = true
	}

	if e.logger != nil {
		e.logger.Infof("Draining node %s before kubelet upgrade", nodeName)
	}
	if err := e.ops.Drain(ctx, nodeName); err != nil {
		if cordonedByUs {
			// We cordoned the node as part of this remediation run; if draining fails we should
			// revert the cordon so the node can continue to accept workloads.
			if e.logger != nil {
				e.logger.WithError(err).Warnf("Drain failed for node %s; uncordoning to restore scheduling", nodeName)
			}
			if uncordonErr := e.ops.Uncordon(ctx, nodeName); uncordonErr != nil {
				return fmt.Errorf("failed to drain node %s: %w (uncordon after drain failure also failed: %v)", nodeName, err, uncordonErr)
			}
		}
		return fmt.Errorf("failed to drain node %s: %w", nodeName, err)
	}

	if e.state != nil {
		e.state.set(nodeName, uncordonAfter)
	}
	return nil
}

type uncordonExecutor struct {
	name   string
	logger *logrus.Logger
	ops    nodeMaintenance
	state  *cordonDrainState
}

func newUncordonExecutor(name string, logger *logrus.Logger, ops nodeMaintenance, state *cordonDrainState) *uncordonExecutor {
	return &uncordonExecutor{name: name, logger: logger, ops: ops, state: state}
}

func (e *uncordonExecutor) GetName() string { return e.name }

func (e *uncordonExecutor) IsCompleted(context.Context) bool { return false }

func (e *uncordonExecutor) Execute(ctx context.Context) error {
	if e.ops == nil {
		return errors.New("node maintenance is nil")
	}

	nodeName, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("failed to get hostname for node maintenance: %w", err)
	}

	if e.state == nil || !e.state.shouldUncordon(nodeName) {
		if e.logger != nil {
			e.logger.Infof("Skipping uncordon for node %s (node was already cordoned)", nodeName)
		}
		return nil
	}

	if e.logger != nil {
		e.logger.Infof("Uncordoning node %s after kubelet upgrade", nodeName)
	}
	if err := e.ops.Uncordon(ctx, nodeName); err != nil {
		return fmt.Errorf("failed to uncordon node %s: %w", nodeName, err)
	}
	return nil
}

// logrusLineWriter writes each line to logrus at a fixed level.
// It intentionally buffers until a newline so kubectl drain helper output is readable.
type logrusLineWriter struct {
	logger *logrus.Logger
	level  logrus.Level

	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *logrusLineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	for _, b := range p {
		if b == '\n' {
			line := w.buf.String()
			w.buf.Reset()
			if w.logger != nil {
				w.logger.Log(w.level, line)
			}
			continue
		}
		_ = w.buf.WriteByte(b)
	}
	return len(p), nil
}
