package k8s

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func makeNode(ready bool) corev1.Node {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return corev1.Node{
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: status},
			},
		},
	}
}

func TestAge(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		when time.Time
		want string
	}{
		{"seconds", now.Add(-30 * time.Second), "30s"},
		{"minutes", now.Add(-5 * time.Minute), "5m"},
		{"hours", now.Add(-3 * time.Hour), "3h"},
		{"days", now.Add(-48 * time.Hour), "2d"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := age(metav1.NewTime(c.when))
			if got != c.want {
				t.Errorf("age(%s) = %q, want %q", c.name, got, c.want)
			}
		})
	}
}

func TestListParamsOptions(t *testing.T) {
	p := ListParams{Limit: 250, Continue: "tok123"}
	opts := p.options()
	if opts.Limit != 250 {
		t.Errorf("Limit = %d, want 250", opts.Limit)
	}
	if opts.Continue != "tok123" {
		t.Errorf("Continue = %q, want tok123", opts.Continue)
	}
}

func TestSplitYAML(t *testing.T) {
	doc := "kind: A\n---\nkind: B\n---\nkind: C\n"
	got := splitYAML(doc)
	if len(got) != 3 {
		t.Fatalf("splitYAML returned %d docs, want 3: %#v", len(got), got)
	}
}

func TestSplitYAMLSingle(t *testing.T) {
	if got := splitYAML("kind: A\n"); len(got) != 1 {
		t.Errorf("single doc split into %d parts, want 1", len(got))
	}
}

func TestKindToGroupKind(t *testing.T) {
	cases := map[string]struct{ group, kind string }{
		"deployments": {"apps", "Deployment"},
		"pods":        {"", "Pod"},
		"ingresses":   {"networking.k8s.io", "Ingress"},
		"cronjobs":    {"batch", "CronJob"},
		"services":    {"", "Service"},
	}
	for in, want := range cases {
		gk := kindToGroupKind(in)
		if gk.Group != want.group || gk.Kind != want.kind {
			t.Errorf("kindToGroupKind(%q) = %+v, want group=%q kind=%q", in, gk, want.group, want.kind)
		}
	}
}

func TestNodeReady(t *testing.T) {
	ready := makeNode(true)
	if !nodeReady(ready) {
		t.Error("expected ready node to report ready")
	}
	notReady := makeNode(false)
	if nodeReady(notReady) {
		t.Error("expected not-ready node to report not ready")
	}
}
