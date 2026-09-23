package daemon

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/config"
	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type nodeLabelClient struct {
	client.Client
	patchErr error
	patches  int
}

func TestProjectedMachineChangesDoNotApplyAKSGoal(t *testing.T) {
	t.Parallel()
	for name, patchErr := range map[string]error{
		"patch allowed":  nil,
		"read-only Node": apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "node1", errors.New("denied")),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			machine := &machinav1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "node1", Generation: 7}}
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1"}}
			c := &nodeLabelClient{Client: fakeClient(machine, node), patchErr: patchErr}
			machines := &fakeMachineClient{machine: &aksmachine.Machine{Goal: aksmachine.GoalState{KubernetesVersion: "1.34.0", SettingsVersion: "42"}}}
			operator := &fakeNodeOperator{state: &State{AppliedSettingsVersion: "41", ActiveMachine: "kube1"}}
			r := newTestRepaveReconciler(t, machines, c, operator)
			for _, generation := range []int64{8, 9} {
				machine.Generation = generation
				if err := c.Update(t.Context(), machine); err != nil {
					t.Fatal(err)
				}
				if err := r.reconcileOnce(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			if operator.applied || operator.reset || operator.stopped || operator.restarted {
				t.Fatal("projected Machine edit or label backfill triggered host mutation")
			}
		})
	}
}

func (c *nodeLabelClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	c.patches++
	if c.patchErr != nil {
		return c.patchErr
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func TestReconcileMachineNodeLabel(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		machine     string
		old         string
		err         error
		wantErr     bool
		wantPatches int
	}{
		"backfill":                         {machine: "arm-worker", wantPatches: 1},
		"replace nspawn identity":          {machine: "arm-worker", old: "kube2", wantPatches: 1},
		"already correct":                  {machine: "arm-worker", old: "arm-worker"},
		"long name removes false identity": {machine: strings.Repeat("a", 63) + ".b", old: "kube1", wantPatches: 1},
		"long name remains unlabeled":      {machine: strings.Repeat("a", 63) + ".b"},
		"read-only permission":             {machine: "arm-worker", err: apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "arm-worker", errors.New("denied")), wantPatches: 1},
		"transient failure":                {machine: "arm-worker", err: apierrors.NewServiceUnavailable("retry"), wantErr: true, wantPatches: 1},
		"node concurrently removed":        {machine: "arm-worker", err: apierrors.NewNotFound(schema.GroupResource{Resource: "nodes"}, "arm-worker"), wantPatches: 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			labels := map[string]string{"customer": "keep", "kubernetes.azure.com/managed": "false", "kubernetes.azure.com/agentpool": "pool", "kubernetes.azure.com/mode": "user", "kubernetes.azure.com/nodepool-type": "FlexNodes"}
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: tt.machine, Labels: maps.Clone(labels), Annotations: map[string]string{"keep": "annotation"}}}
			if tt.old != "" {
				node.Labels[config.MachineNodeLabel] = tt.old
			}
			c := &nodeLabelClient{Client: fakeClient(node), patchErr: tt.err}
			r := &repaveReconciler{client: c, nodeName: tt.machine, log: slog.Default()}
			var latest corev1.Node
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(node), &latest); err != nil {
				t.Fatal(err)
			}
			if err := r.reconcileMachineNodeLabel(t.Context(), &latest); (err != nil) != tt.wantErr {
				t.Fatalf("error = %v", err)
			}
			if c.patches != tt.wantPatches {
				t.Fatalf("patches = %d, want %d", c.patches, tt.wantPatches)
			}
			if tt.err != nil {
				return
			}
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(node), &latest); err != nil {
				t.Fatal(err)
			}
			want := tt.machine
			if len(want) > 63 {
				want = ""
			}
			if latest.Labels[config.MachineNodeLabel] != want || latest.Annotations["keep"] != "annotation" {
				t.Fatalf("node metadata = %#v", latest.ObjectMeta)
			}
			for k, v := range labels {
				if latest.Labels[k] != v {
					t.Fatalf("label %s changed", k)
				}
			}
			if err := r.reconcileMachineNodeLabel(t.Context(), &latest); err != nil || c.patches != tt.wantPatches {
				t.Fatalf("second reconciliation: patches = %d, error = %v", c.patches, err)
			}
		})
	}
}
