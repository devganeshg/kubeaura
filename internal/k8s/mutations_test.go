package k8s

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

// These cover the operations that change a cluster. Everything else in this
// package reads; these five can lose someone's workload, so their dispatch and
// their refusal to act on unknown input are worth pinning down.

func testClient(objs ...runtime.Object) *Client {
	cs := fake.NewSimpleClientset(objs...)

	// client-go's fake does not back the scale subresource with the stored
	// Deployment, so GetScale/UpdateScale need reactors to behave like a real
	// API server. Without these, ScaleDeployment cannot be tested at all.
	cs.PrependReactor("get", "deployments", func(a ktesting.Action) (bool, runtime.Object, error) {
		ga, ok := a.(ktesting.GetActionImpl)
		if !ok || ga.Subresource != "scale" {
			return false, nil, nil
		}
		d, err := cs.Tracker().Get(
			schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
			ga.Namespace, ga.Name)
		if err != nil {
			return true, nil, err
		}
		dep := d.(*appsv1.Deployment)
		var n int32
		if dep.Spec.Replicas != nil {
			n = *dep.Spec.Replicas
		}
		return true, &autoscalingv1.Scale{
			ObjectMeta: metav1.ObjectMeta{Namespace: dep.Namespace, Name: dep.Name},
			Spec:       autoscalingv1.ScaleSpec{Replicas: n},
		}, nil
	})
	cs.PrependReactor("update", "deployments", func(a ktesting.Action) (bool, runtime.Object, error) {
		ua, ok := a.(ktesting.UpdateActionImpl)
		if !ok || ua.Subresource != "scale" {
			return false, nil, nil
		}
		sc := ua.Object.(*autoscalingv1.Scale)
		gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
		d, err := cs.Tracker().Get(gvr, sc.Namespace, sc.Name)
		if err != nil {
			return true, nil, err
		}
		dep := d.(*appsv1.Deployment)
		n := sc.Spec.Replicas
		dep.Spec.Replicas = &n
		return true, sc, cs.Tracker().Update(gvr, dep, dep.Namespace)
	})

	return &Client{cs: cs, Context: "test"}
}

func deployment(ns, name string, replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{}},
			},
		},
	}
}

func TestScaleDeployment(t *testing.T) {
	c := testClient(deployment("demo", "web", 1))

	if err := c.ScaleDeployment("demo", "web", 3); err != nil {
		t.Fatalf("ScaleDeployment: %v", err)
	}
	got, err := c.cs.AppsV1().Deployments("demo").Get(context.Background(), "web", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Spec.Replicas == nil || *got.Spec.Replicas != 3 {
		t.Errorf("replicas = %v, want 3", got.Spec.Replicas)
	}

	// Scaling to zero is a legitimate operation, not a no-op to be swallowed.
	if err := c.ScaleDeployment("demo", "web", 0); err != nil {
		t.Fatalf("scale to zero: %v", err)
	}
	got, _ = c.cs.AppsV1().Deployments("demo").Get(context.Background(), "web", metav1.GetOptions{})
	if got.Spec.Replicas == nil || *got.Spec.Replicas != 0 {
		t.Errorf("replicas = %v, want 0", got.Spec.Replicas)
	}

	if err := c.ScaleDeployment("demo", "does-not-exist", 2); err == nil {
		t.Error("scaling a missing deployment should error, not silently succeed")
	}
}

func TestRestartDeployment(t *testing.T) {
	c := testClient(deployment("demo", "web", 2))

	if err := c.RestartDeployment("demo", "web"); err != nil {
		t.Fatalf("RestartDeployment: %v", err)
	}
	got, err := c.cs.AppsV1().Deployments("demo").Get(context.Background(), "web", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// A restart is a rollout trigger: it stamps the pod template and must not
	// change the replica count.
	if got.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"] == "" {
		t.Error("restart did not stamp the restartedAt annotation, so no rollout happens")
	}
	if got.Spec.Replicas == nil || *got.Spec.Replicas != 2 {
		t.Errorf("restart changed replicas to %v, want 2 untouched", got.Spec.Replicas)
	}

	if err := c.RestartDeployment("demo", "nope"); err == nil {
		t.Error("restarting a missing deployment should error")
	}
}

func TestDeleteResource(t *testing.T) {
	tests := []struct {
		kind   string
		exists func(*Client) bool
	}{
		{"pod", func(c *Client) bool { return getErr(c, "pod") == nil }},
		{"pods", func(c *Client) bool { return getErr(c, "pod") == nil }},
		{"deployment", func(c *Client) bool { return getErr(c, "deployment") == nil }},
		{"service", func(c *Client) bool { return getErr(c, "service") == nil }},
		{"configmap", func(c *Client) bool { return getErr(c, "configmap") == nil }},
		{"job", func(c *Client) bool { return getErr(c, "job") == nil }},
	}

	for _, tc := range tests {
		t.Run(tc.kind, func(t *testing.T) {
			c := seeded()
			if !tc.exists(c) {
				t.Fatalf("fixture missing before delete")
			}
			if err := c.DeleteResource(tc.kind, "demo", "thing"); err != nil {
				t.Fatalf("DeleteResource(%q): %v", tc.kind, err)
			}
			if tc.exists(c) {
				t.Errorf("DeleteResource(%q) reported success but the object is still there", tc.kind)
			}
		})
	}
}

func TestDeleteResourceRefusesUnknownKind(t *testing.T) {
	c := seeded()

	// An unknown kind must be refused loudly. Falling through to a no-op would
	// tell the UI a delete succeeded when nothing happened.
	for _, kind := range []string{"", "secret", "clusterrole", "../pods", "Pod;drop"} {
		err := c.DeleteResource(kind, "demo", "thing")
		if err == nil {
			t.Errorf("DeleteResource(%q) returned nil; unsupported kinds must error", kind)
			continue
		}
		if !strings.Contains(err.Error(), "not supported") {
			t.Errorf("DeleteResource(%q) error = %q, want it to say the kind is unsupported", kind, err)
		}
	}
}

func TestDeleteResourceMissingObject(t *testing.T) {
	c := seeded()
	if err := c.DeleteResource("pod", "demo", "absent"); err == nil {
		t.Error("deleting a missing pod should surface the API's NotFound, not succeed")
	}
}

// --- helpers ---

// seeded returns a client holding one object of each deletable kind, all named
// "thing" in namespace "demo".
func seeded() *Client {
	meta := metav1.ObjectMeta{Namespace: "demo", Name: "thing"}
	return testClient(
		&corev1.Pod{ObjectMeta: meta},
		&appsv1.Deployment{ObjectMeta: meta},
		&corev1.Service{ObjectMeta: meta},
		&corev1.ConfigMap{ObjectMeta: meta},
		&batchv1.Job{ObjectMeta: meta},
	)
}

func getErr(c *Client, kind string) error {
	cx := context.Background()
	opts := metav1.GetOptions{}
	switch kind {
	case "pod":
		_, err := c.cs.CoreV1().Pods("demo").Get(cx, "thing", opts)
		return err
	case "deployment":
		_, err := c.cs.AppsV1().Deployments("demo").Get(cx, "thing", opts)
		return err
	case "service":
		_, err := c.cs.CoreV1().Services("demo").Get(cx, "thing", opts)
		return err
	case "configmap":
		_, err := c.cs.CoreV1().ConfigMaps("demo").Get(cx, "thing", opts)
		return err
	case "job":
		_, err := c.cs.BatchV1().Jobs("demo").Get(cx, "thing", opts)
		return err
	}
	return nil
}
