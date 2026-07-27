package k8s

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// helmSecretFor builds a storage secret in exactly the layout Helm v3 writes:
// the release JSON gzipped, then base64-encoded, then stored as secret data.
func helmSecretFor(t *testing.T, name, namespace string, revision int, status, chartVer, manifest string, values map[string]interface{}) corev1.Secret {
	t.Helper()
	rel := map[string]interface{}{
		"name": name,
		"info": map[string]interface{}{
			"first_deployed": "2026-07-01T10:00:00Z",
			"last_deployed":  "2026-07-20T10:00:00Z",
			"status":         status,
			"description":    "Upgrade complete",
			"notes":          "Thanks for installing " + name,
		},
		"chart": map[string]interface{}{
			"metadata": map[string]interface{}{
				"name":        "demo",
				"version":     chartVer,
				"appVersion":  "9.9",
				"description": "a demo chart",
				"home":        "https://example.invalid",
			},
			"values": map[string]interface{}{
				"replicas": 1,
				"image":    map[string]interface{}{"tag": "v1", "repo": "demo"},
			},
		},
		"config":    values,
		"manifest":  manifest,
		"version":   revision,
		"namespace": namespace,
	}
	raw, err := json.Marshal(rel)
	if err != nil {
		t.Fatalf("marshal release: %v", err)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(raw); err != nil {
		t.Fatalf("gzip release: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	encoded := make([]byte, base64.StdEncoding.EncodedLen(gz.Len()))
	base64.StdEncoding.Encode(encoded, gz.Bytes())

	return corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sh.helm.release.v1." + name + ".v" + itoa(revision),
			Namespace: namespace,
			Labels: map[string]string{
				"owner":   "helm",
				"name":    name,
				"version": itoa(revision),
				"status":  status,
			},
			CreationTimestamp: metav1.NewTime(time.Now().Add(-2 * time.Hour)),
		},
		Type: corev1.SecretType(helmSecretType),
		Data: map[string][]byte{"release": encoded},
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

const demoManifest = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: demo
spec:
  replicas: 1
---
apiVersion: v1
kind: Service
metadata:
  name: demo-svc
`

// helmTestClient builds a Client over the fake clientset. The fake does not
// honour field selectors, which is fine: helmSecrets filters on the payload too.
func helmTestClient(objs ...corev1.Secret) *Client {
	cs := fake.NewSimpleClientset()
	for i := range objs {
		_, _ = cs.CoreV1().Secrets(objs[i].Namespace).Create(context.Background(), &objs[i], metav1.CreateOptions{})
	}
	return &Client{cs: cs, Context: "test-ctx"}
}

func TestHelmReleasesReportsOnlyLatestRevision(t *testing.T) {
	c := helmTestClient(
		helmSecretFor(t, "demo", "default", 1, "superseded", "1.0.0", demoManifest, nil),
		helmSecretFor(t, "demo", "default", 2, "deployed", "1.1.0", demoManifest, nil),
		helmSecretFor(t, "other", "prod", 1, "deployed", "2.0.0", demoManifest, nil),
	)
	res, err := c.HelmReleases(context.Background(), "")
	if err != nil {
		t.Fatalf("HelmReleases: %v", err)
	}
	if len(res.Releases) != 2 {
		t.Fatalf("want 2 releases (latest revision of each), got %d: %+v", len(res.Releases), res.Releases)
	}
	var demo *HelmRelease
	for i := range res.Releases {
		if res.Releases[i].Name == "demo" {
			demo = &res.Releases[i]
		}
	}
	if demo == nil {
		t.Fatal("demo release missing")
	}
	if demo.Revision != 2 {
		t.Errorf("revision = %d, want 2 (the latest)", demo.Revision)
	}
	if demo.Status != "Deployed" {
		t.Errorf("status = %q, want %q", demo.Status, "Deployed")
	}
	if demo.Chart != "demo-1.1.0" {
		t.Errorf("chart = %q, want %q", demo.Chart, "demo-1.1.0")
	}
	if demo.AppVersion != "9.9" {
		t.Errorf("appVersion = %q, want %q", demo.AppVersion, "9.9")
	}
}

func TestHelmReleaseDetailDecodesValuesNotesAndHistory(t *testing.T) {
	c := helmTestClient(
		helmSecretFor(t, "demo", "default", 1, "superseded", "1.0.0", demoManifest, nil),
		helmSecretFor(t, "demo", "default", 2, "deployed", "1.1.0", demoManifest,
			map[string]interface{}{"replicas": 3, "image": map[string]interface{}{"tag": "v2"}}),
	)
	d, err := c.HelmReleaseDetail(context.Background(), "default", "demo", 0)
	if err != nil {
		t.Fatalf("HelmReleaseDetail: %v", err)
	}
	if d.Revision != 2 {
		t.Errorf("revision = %d, want 2 (0 means latest)", d.Revision)
	}
	if !strings.Contains(d.Notes, "Thanks for installing demo") {
		t.Errorf("notes not decoded: %q", d.Notes)
	}
	if len(d.History) != 2 {
		t.Errorf("history has %d entries, want 2", len(d.History))
	} else if d.History[0].Revision != 2 {
		t.Errorf("history is not newest-first: %+v", d.History)
	}
	if !strings.Contains(d.UserValues, "replicas: 3") {
		t.Errorf("user values missing the override:\n%s", d.UserValues)
	}
	// The merged view must keep chart defaults the user did not override.
	if !strings.Contains(d.AllValues, "repo: demo") {
		t.Errorf("merged values dropped a chart default:\n%s", d.AllValues)
	}
	if !strings.Contains(d.AllValues, "tag: v2") {
		t.Errorf("merged values did not apply the override:\n%s", d.AllValues)
	}
	if len(d.Resources) != 2 {
		t.Fatalf("want 2 manifest objects, got %d: %+v", len(d.Resources), d.Resources)
	}
	if d.Resources[0].Kind != "Deployment" || d.Resources[0].Namespace != "default" {
		t.Errorf("first manifest object = %+v, want a Deployment in default", d.Resources[0])
	}
}

func TestHelmReleaseDetailSelectsRequestedRevision(t *testing.T) {
	c := helmTestClient(
		helmSecretFor(t, "demo", "default", 1, "superseded", "1.0.0", demoManifest, nil),
		helmSecretFor(t, "demo", "default", 2, "deployed", "1.1.0", demoManifest, nil),
	)
	d, err := c.HelmReleaseDetail(context.Background(), "default", "demo", 1)
	if err != nil {
		t.Fatalf("HelmReleaseDetail: %v", err)
	}
	if d.Revision != 1 || d.ChartVer != "1.0.0" {
		t.Errorf("got revision %d chart %s, want revision 1 chart 1.0.0", d.Revision, d.ChartVer)
	}
}

func TestHelmReleaseDetailMissingRelease(t *testing.T) {
	c := helmTestClient()
	if _, err := c.HelmReleaseDetail(context.Background(), "default", "nope", 0); err == nil {
		t.Fatal("want an error for a release that does not exist")
	}
}

func TestHelmDiffComparesRevisionManifests(t *testing.T) {
	v2 := strings.Replace(demoManifest, "replicas: 1", "replicas: 5", 1)
	c := helmTestClient(
		helmSecretFor(t, "demo", "default", 1, "superseded", "1.0.0", demoManifest, nil),
		helmSecretFor(t, "demo", "default", 2, "deployed", "1.1.0", v2, nil),
	)
	d, err := c.HelmDiff(context.Background(), "default", "demo", 1, 2)
	if err != nil {
		t.Fatalf("HelmDiff: %v", err)
	}
	if !strings.Contains(d.Live, "replicas: 1") || !strings.Contains(d.Proposed, "replicas: 5") {
		t.Errorf("diff sides are wrong:\nlive:\n%s\nproposed:\n%s", d.Live, d.Proposed)
	}
}

// A corrupt revision must not hide the readable ones — history is exactly what
// an operator needs when a release has gone wrong.
func TestHelmReleaseDetailSkipsUndecodableRevision(t *testing.T) {
	bad := helmSecretFor(t, "demo", "default", 3, "failed", "1.2.0", demoManifest, nil)
	bad.Data["release"] = []byte("!!!not base64 or gzip or json!!!")
	c := helmTestClient(
		helmSecretFor(t, "demo", "default", 1, "superseded", "1.0.0", demoManifest, nil),
		helmSecretFor(t, "demo", "default", 2, "deployed", "1.1.0", demoManifest, nil),
		bad,
	)
	d, err := c.HelmReleaseDetail(context.Background(), "default", "demo", 0)
	if err != nil {
		t.Fatalf("HelmReleaseDetail: %v", err)
	}
	if len(d.History) != 2 {
		t.Errorf("history has %d entries, want the 2 readable ones", len(d.History))
	}
	if d.Revision != 2 {
		t.Errorf("latest resolved to revision %d, want 2", d.Revision)
	}
}

func TestDecodeHelmSecretAcceptsPlainJSON(t *testing.T) {
	// Not every writer gzips; the decoder detects the layers by content.
	raw := []byte(`{"name":"plain","version":7,"info":{"status":"deployed"}}`)
	rel, err := decodeHelmSecret(raw)
	if err != nil {
		t.Fatalf("decodeHelmSecret: %v", err)
	}
	if rel.Name != "plain" || rel.Version != 7 {
		t.Errorf("decoded %+v, want name=plain version=7", rel)
	}
}

func TestHelmArgsRejectsInjectedFlags(t *testing.T) {
	cases := []struct {
		name string
		op   HelmOp
	}{
		{"release as flag", HelmOp{Action: "upgrade", Release: "--kubeconfig=/etc/evil", Chart: "repo/chart"}},
		{"chart as flag", HelmOp{Action: "install", Release: "ok", Chart: "--post-renderer=/bin/sh"}},
		{"namespace as flag", HelmOp{Action: "uninstall", Release: "ok", Namespace: "--kube-apiserver=x"}},
		{"unknown action", HelmOp{Action: "template", Release: "ok", Chart: "repo/chart"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, cleanup, err := helmArgs("ctx", c.op); err == nil {
				if cleanup != nil {
					cleanup()
				}
				t.Fatalf("helmArgs accepted %+v", c.op)
			}
		})
	}
}

func TestHelmArgsBuildsExpectedInvocation(t *testing.T) {
	args, cleanup, err := helmArgs("prod-ctx", HelmOp{
		Action: "upgrade", Release: "demo", Namespace: "web",
		Chart: "bitnami/nginx", Version: "1.2.3", Wait: true, Atomic: true,
	})
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		t.Fatalf("helmArgs: %v", err)
	}
	got := strings.Join(args, " ")
	for _, want := range []string{
		"upgrade --install demo bitnami/nginx",
		"--version 1.2.3",
		"--namespace web",
		"--kube-context prod-ctx",
		"--wait",
		"--atomic",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("args %q missing %q", got, want)
		}
	}
}

// An in-cluster run has no kubeconfig context to name, so passing one would
// make every helm call fail.
func TestHelmArgsOmitsKubeContextInCluster(t *testing.T) {
	args, cleanup, err := helmArgs("in-cluster", HelmOp{Action: "uninstall", Release: "demo", Namespace: "web"})
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		t.Fatalf("helmArgs: %v", err)
	}
	if strings.Contains(strings.Join(args, " "), "--kube-context") {
		t.Errorf("args should not carry --kube-context in-cluster: %v", args)
	}
}

func TestHelmArgsRedactsValuesPath(t *testing.T) {
	args, cleanup, err := helmArgs("ctx", HelmOp{
		Action: "install", Release: "demo", Chart: "repo/chart", Values: "replicas: 2\n",
	})
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		t.Fatalf("helmArgs: %v", err)
	}
	echoed := strings.Join(redactHelmArgs(args), " ")
	if !strings.Contains(echoed, "--values <values.yaml>") {
		t.Errorf("values path was not redacted: %q", echoed)
	}
}
