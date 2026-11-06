package yurthub

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func initTestMetadata(t *testing.T, client *fake.Clientset) error {
	t.Helper()
	return initializeMetadata(t.Context(), client.CoreV1().Nodes(), "node1", "uid1", "pool1")
}

func TestInitializeMetadataPreservesPolicy(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"missing", "customer values", "customer empty values", "completed with removal", "customer finite duration only", "customer legacy only"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1", UID: "uid1", ResourceVersion: "1", Annotations: map[string]string{"customer": "keep"}}}
			switch name {
			case "customer values", "customer empty values":
				value := "11m"
				if name == "customer empty values" {
					value = ""
				}
				node.Annotations["node.openyurt.io/autonomy-duration"] = value
				node.Annotations["node.beta.openyurt.io/autonomy"] = "false"
				node.Labels = map[string]string{"openyurt.io/is-edge-worker": "false", "apps.openyurt.io/nodepool": "customer-pool"}
			case "completed with removal":
				node.Annotations[completionKey] = "uid1"
			case "customer finite duration only":
				node.Annotations[durationAnnotation] = "7m"
			case "customer legacy only":
				node.Annotations[legacyAutonomyAnnotation] = "false"
			}
			client := fake.NewClientset(node)
			for range 2 {
				if err := initTestMetadata(t, client); err != nil {
					t.Fatal(err)
				}
			}
			got, err := client.CoreV1().Nodes().Get(t.Context(), "node1", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if got.Annotations["customer"] != "keep" || got.Annotations[completionKey] != "uid1" {
				t.Fatalf("annotations = %v", got.Annotations)
			}
			// The deprecated annotation overrides any duration in the manager, so
			// the agent never adds it; a customer-set value is left as is.
			if _, added := got.Annotations[legacyAutonomyAnnotation]; added && node.Annotations[legacyAutonomyAnnotation] == "" {
				t.Fatal("agent added the deprecated autonomy annotation")
			}
			if name == "customer legacy only" {
				if _, exists := got.Annotations[durationAnnotation]; exists {
					t.Fatal("added a duration over an explicit legacy customer setting")
				}
			}
			if name == "missing" {
				if got.Annotations[durationAnnotation] != autonomyDuration || got.Labels["openyurt.io/is-edge-worker"] != "true" {
					t.Fatalf("defaults missing: %+v", got.ObjectMeta)
				}
			} else {
				for k, v := range node.Annotations {
					if got.Annotations[k] != v {
						t.Errorf("customer annotation %s overwritten", k)
					}
				}
				if node.Labels != nil && !reflect.DeepEqual(got.Labels, node.Labels) {
					t.Errorf("customer labels changed: %v", got.Labels)
				}
				if name == "completed with removal" {
					if _, exists := got.Annotations["node.openyurt.io/autonomy-duration"]; exists {
						t.Fatal("deliberate removal was reapplied")
					}
				}
			}
			patches := 0
			for _, action := range client.Actions() {
				if action.GetVerb() == "patch" {
					patches++
				}
			}
			want := 1
			if name == "completed with removal" {
				want = 0
			}
			if patches != want {
				t.Fatalf("patches = %d, want %d", patches, want)
			}
		})
	}
}

func TestInitializeMetadataConcurrentEdit(t *testing.T) {
	t.Parallel()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1", UID: "uid1", ResourceVersion: "1"}}
	client := fake.NewClientset(node)
	patches := 0
	client.PrependReactor("patch", "nodes", func(action clienttesting.Action) (bool, runtime.Object, error) {
		patches++
		patch := action.(clienttesting.PatchAction).GetPatch()
		var body struct {
			Metadata metav1.ObjectMeta `json:"metadata"`
		}
		if err := json.Unmarshal(patch, &body); err != nil {
			t.Fatal(err)
		}
		if body.Metadata.UID != "uid1" || body.Metadata.ResourceVersion == "" || body.Metadata.Annotations[completionKey] != "uid1" {
			t.Fatalf("missing identity/concurrency/completion guard: %s", patch)
		}
		if patches == 1 {
			updated := node.DeepCopy()
			updated.ResourceVersion = "2"
			updated.Annotations = map[string]string{"node.openyurt.io/autonomy-duration": "9m", "customer": "concurrent"}
			if err := client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("nodes"), updated, ""); err != nil {
				t.Fatal(err)
			}
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "nodes"}, "node1", errors.New("concurrent edit"))
		}
		if body.Metadata.ResourceVersion != "2" {
			t.Fatal("retry used stale resourceVersion")
		}
		if _, exists := body.Metadata.Annotations["node.openyurt.io/autonomy-duration"]; exists {
			t.Fatal("retry would overwrite concurrent customer choice")
		}
		return false, nil, nil
	})
	if err := initTestMetadata(t, client); err != nil {
		t.Fatal(err)
	}
	got, err := client.CoreV1().Nodes().Get(t.Context(), "node1", metav1.GetOptions{})
	if err != nil || got.Annotations["node.openyurt.io/autonomy-duration"] != "9m" || patches != 2 {
		t.Fatalf("concurrent edit lost: %+v, %v, patches=%d", got, err, patches)
	}
}

func TestInitializeMetadataRejectsUnsafeInitialization(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"replacement", "deleting", "marker mismatch", "forbidden", "replacement during retry"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1", UID: "uid1", ResourceVersion: "1"}}
			switch name {
			case "replacement":
				node.UID = "uid2"
			case "deleting":
				now := metav1.Now()
				node.DeletionTimestamp = &now
			case "marker mismatch":
				node.Annotations = map[string]string{completionKey: "uid2"}
			}
			client := fake.NewClientset(node)
			patches := 0
			client.PrependReactor("patch", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
				patches++
				if name == "replacement during retry" {
					replacement := node.DeepCopy()
					replacement.UID = types.UID("uid2")
					if err := client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("nodes"), replacement, ""); err != nil {
						t.Fatal(err)
					}
					return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "nodes"}, "node1", errors.New("replaced"))
				}
				return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "node1", errors.New("denied"))
			})
			err := initTestMetadata(t, client)
			if err == nil {
				t.Fatal("expected initialization failure")
			}
			if name == "forbidden" && !apierrors.IsForbidden(err) {
				t.Fatalf("authorization failure not preserved: %v", err)
			}
			want := 0
			if name == "forbidden" || name == "replacement during retry" {
				want = 1
			}
			if patches != want {
				t.Fatalf("patches = %d, want %d", patches, want)
			}
		})
	}
}
