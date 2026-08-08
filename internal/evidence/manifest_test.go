package evidence

import (
	"fmt"
	"strings"
	"testing"
)

// As with evidence_test.go, these assert against the payload text: what matters
// is the bytes that leave, not the shape they passed through on the way out.

const secretManifest = `apiVersion: v1
kind: Secret
metadata:
  name: app-secrets
  namespace: payments
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: '{"data":{"password":"aHVudGVyMg=="}}'
type: Opaque
data:
  DB_PASSWORD: aHVudGVyMg==
  API_TOKEN: c2stbGl2ZS1kZWFkYmVlZg==
stringData:
  plain: hunter2
`

func TestForManifestRemovesSecretBodyKeepsKeys(t *testing.T) {
	p, err := ForManifest(ManifestInput{YAML: secretManifest, Kind: "Secret", Namespace: "payments", Name: "app-secrets"})
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"aHVudGVyMg==", "c2stbGl2ZS1kZWFkYmVlZg==", "hunter2"} {
		if strings.Contains(p.JSON, leak) {
			t.Errorf("secret value %q survived redaction:\n%s", leak, p.JSON)
		}
	}
	// The keys are the point of a review: the operator needs to see which keys
	// a Secret defines to reason about a workload that reads them.
	for _, keep := range []string{"DB_PASSWORD", "API_TOKEN", "plain", "app-secrets"} {
		if !strings.Contains(p.JSON, keep) {
			t.Errorf("expected %q to survive, payload:\n%s", keep, p.JSON)
		}
	}
	if !hasRule(p.Envelope.Redactions, "secret-data") {
		t.Errorf("expected a secret-data redaction, got %+v", p.Envelope.Redactions)
	}
	if p.Envelope.Purpose != "review" {
		t.Errorf("purpose = %q, want review", p.Envelope.Purpose)
	}
	if p.Envelope.Bytes != len(p.JSON) {
		t.Errorf("Bytes = %d, want %d", p.Envelope.Bytes, len(p.JSON))
	}
}

func TestForManifestDropsLastAppliedAnnotation(t *testing.T) {
	// last-applied-configuration is a verbatim copy of the original manifest,
	// so leaving it in would re-admit everything the other rules removed.
	p, err := ForManifest(ManifestInput{YAML: secretManifest, Kind: "Secret"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p.JSON, "last-applied-configuration") {
		t.Errorf("last-applied annotation survived:\n%s", p.JSON)
	}
}

func TestForManifestScrubsConfigMapValues(t *testing.T) {
	// A ConfigMap is not a Secret, so its values are sent — but a ConfigMap is
	// also where a leaked credential most often actually lives.
	const cm = `apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
data:
  app.conf: |
    log_level=debug
    DB_PASSWORD=hunter2
    endpoint=https://api.internal
`
	p, err := ForManifest(ManifestInput{YAML: cm, Kind: "ConfigMap", Name: "app-config"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p.JSON, "hunter2") {
		t.Errorf("configmap credential survived:\n%s", p.JSON)
	}
	if !strings.Contains(p.JSON, "log_level=debug") {
		t.Errorf("scrubbing over-reached and ate benign config:\n%s", p.JSON)
	}
	if !strings.Contains(p.JSON, "DB_PASSWORD") {
		t.Errorf("the key should survive so the model can still reason about it:\n%s", p.JSON)
	}
}

func TestForManifestRedactsInlineEnvInWorkloads(t *testing.T) {
	// The ForPod guarantee, applied structurally: it must hold for a Deployment
	// and for a pod template nested at any depth, not just for a live Pod.
	const dep = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
spec:
  template:
    spec:
      containers:
      - name: api
        image: ghcr.io/acme/api:1.4.2
        env:
        - name: DB_PASSWORD
          value: hunter2
        - name: DB_HOST
          valueFrom:
            secretKeyRef:
              name: app-secrets
              key: host
`
	p, err := ForManifest(ManifestInput{YAML: dep, Kind: "Deployment", Name: "api"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p.JSON, "hunter2") {
		t.Errorf("inline env value survived:\n%s", p.JSON)
	}
	if !strings.Contains(p.JSON, "DB_PASSWORD") || !strings.Contains(p.JSON, redactedMark) {
		t.Errorf("expected the name kept and the value marked:\n%s", p.JSON)
	}
	// A secretKeyRef is a reference, not a body — it must survive, because a
	// diagnosis frequently turns on the Secret it points at.
	if !strings.Contains(p.JSON, "app-secrets") {
		t.Errorf("secretKeyRef target should survive:\n%s", p.JSON)
	}
	if !hasRule(p.Envelope.Redactions, "env-value") {
		t.Errorf("expected an env-value redaction, got %+v", p.Envelope.Redactions)
	}
}

func TestForManifestScrubsUnparseableInput(t *testing.T) {
	// A syntax error is a legitimate thing to ask the model about, so the
	// document is still sent — but it cannot be walked, so it gets the
	// free-text scrubbers and is labelled as unparsed.
	const broken = "kind: Deployment\n  bad indent: [\npassword=hunter2\n"
	p, err := ForManifest(ManifestInput{YAML: broken})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p.JSON, "hunter2") {
		t.Errorf("credential survived in unparsed manifest:\n%s", p.JSON)
	}
	if !strings.Contains(strings.Join(p.Envelope.Fields, ","), "unparsed") {
		t.Errorf("envelope should disclose that the document was not parsed: %v", p.Envelope.Fields)
	}
}

func TestForManifestRejectsEmpty(t *testing.T) {
	if _, err := ForManifest(ManifestInput{YAML: "   \n"}); err == nil {
		t.Fatal("expected an error for an empty manifest")
	}
}

func TestForManifestCapsAndReportsTruncation(t *testing.T) {
	var b strings.Builder
	b.WriteString("kind: ConfigMap\ndata:\n")
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&b, "  key%05d: %s\n", i, strings.Repeat("v", 20))
	}
	p, err := ForManifest(ManifestInput{YAML: b.String(), Kind: "ConfigMap"})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.JSON) > MaxManifestBytes {
		t.Errorf("payload %d bytes exceeds cap %d", len(p.JSON), MaxManifestBytes)
	}
	if !p.Envelope.Truncated {
		t.Error("envelope should report truncation")
	}
}

func TestForManifestHashIsDeterministic(t *testing.T) {
	a, err := ForManifest(ManifestInput{YAML: secretManifest, Kind: "Secret"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := ForManifest(ManifestInput{YAML: secretManifest, Kind: "Secret"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Envelope.Hash != b.Envelope.Hash {
		t.Errorf("hash not stable: %s vs %s", a.Envelope.Hash, b.Envelope.Hash)
	}
	if a.Envelope.Hash != hashOf([]byte(a.JSON)) {
		t.Error("hash does not cover the exact payload")
	}
}

func hasRule(rs []Redaction, rule string) bool {
	for _, r := range rs {
		if r.Rule == rule {
			return true
		}
	}
	return false
}
