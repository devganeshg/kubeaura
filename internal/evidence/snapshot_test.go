package evidence

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestForSnapshotScrubsEventMessages(t *testing.T) {
	// An event message is verbatim controller or application output. This is
	// the case the structured rules cannot reach, so the scrubbers must.
	p, err := ForSnapshot(SnapshotInput{
		Namespace: "payments",
		Summary:   map[string]int{"pods": 3},
		Groups: []SnapshotGroup{{Kind: "events", Items: []SnapshotItem{{
			Kind: "Event", Name: "api-7d9", Namespace: "payments",
			Info: `Failed: dial postgres://admin:hunter2@db:5432 refused`,
		}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p.JSON, "hunter2") {
		t.Errorf("credential in event message survived:\n%s", p.JSON)
	}
	// Scrubbing must not eat the diagnosis: the host and the failure mode are
	// the whole reason the event is being sent.
	if !strings.Contains(p.JSON, "refused") {
		t.Errorf("scrubbing over-reached:\n%s", p.JSON)
	}
	if !hasRule(p.Envelope.Redactions, "free-text-secret") {
		t.Errorf("expected a free-text-secret redaction, got %+v", p.Envelope.Redactions)
	}
}

func TestForSnapshotRedactsSensitiveLabels(t *testing.T) {
	p, err := ForSnapshot(SnapshotInput{
		Summary: map[string]int{},
		Groups: []SnapshotGroup{{Kind: "pods", Items: []SnapshotItem{{
			Kind: "Pod", Name: "api", Labels: map[string]string{
				"app":          "api",
				"deploy-token": "ghp_0123456789abcdefghijABCDEFGHIJ",
			},
		}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p.JSON, "ghp_0123456789abcdefghijABCDEFGHIJ") {
		t.Errorf("label token survived:\n%s", p.JSON)
	}
	if !strings.Contains(p.JSON, `"app":"api"`) {
		t.Errorf("benign label should survive:\n%s", p.JSON)
	}
	if !hasRule(p.Envelope.Redactions, "sensitive-label") {
		t.Errorf("expected a sensitive-label redaction, got %+v", p.Envelope.Redactions)
	}
}

func TestForSnapshotCapsBytesAndKeepsNewest(t *testing.T) {
	// 5 kinds x 200 rows had no ceiling before; a large namespace produced a
	// payload that was expensive and useless at the same time.
	big := make([]SnapshotItem, 4000)
	for i := range big {
		big[i] = SnapshotItem{
			Kind: "Pod", Namespace: "payments",
			Name:   fmt.Sprintf("worker-%04d", i),
			Status: "Running",
			Info:   strings.Repeat("filler ", 10),
		}
	}
	p, err := ForSnapshot(SnapshotInput{
		Summary: map[string]int{"pods": len(big)},
		Groups: []SnapshotGroup{
			{Kind: "nodes", Items: []SnapshotItem{{Kind: "Node", Name: "ip-10-0-3-14"}}},
			{Kind: "pods", Items: big},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Envelope.Bytes > MaxSnapshotBytes {
		t.Errorf("payload %d bytes exceeds cap %d", p.Envelope.Bytes, MaxSnapshotBytes)
	}
	if !p.Envelope.Truncated {
		t.Error("envelope should report truncation")
	}
	if p.Envelope.Items >= len(big) {
		t.Errorf("Items = %d, expected fewer than %d after trimming", p.Envelope.Items, len(big))
	}
	// Trimming halves the largest group, so the small load-bearing groups
	// survive: losing the node list to make room for pod 3,999 is the wrong
	// trade for a diagnosis.
	if !strings.Contains(p.JSON, "ip-10-0-3-14") {
		t.Error("the small group should survive trimming")
	}
	if !strings.Contains(p.JSON, "worker-0000") {
		t.Error("trimming should keep the head of the list, dropping from the tail")
	}
}

func TestForSnapshotEnvelopeDescribesPayload(t *testing.T) {
	p, err := ForSnapshot(SnapshotInput{
		Purpose:   "triage",
		Namespace: "payments",
		Summary:   map[string]int{"pods": 1},
		Groups:    []SnapshotGroup{{Kind: "alerts", Items: []SnapshotItem{{Kind: "Pod", Name: "api"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Envelope.Purpose != "triage" {
		t.Errorf("purpose = %q, want triage", p.Envelope.Purpose)
	}
	if p.Envelope.Resource.Namespace != "payments" {
		t.Errorf("namespace = %q", p.Envelope.Resource.Namespace)
	}
	if p.Envelope.Items != 1 {
		t.Errorf("Items = %d, want 1", p.Envelope.Items)
	}
	if p.Envelope.Bytes != len(p.JSON) {
		t.Errorf("Bytes = %d, want %d", p.Envelope.Bytes, len(p.JSON))
	}
	// The payload must stay valid JSON: it is interpolated into a prompt that
	// tells the model it is reading JSON.
	var out map[string]interface{}
	if err := json.Unmarshal([]byte(p.JSON), &out); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}
	if _, ok := out["alerts"]; !ok {
		t.Errorf("group missing from payload: %v", out)
	}
}

func TestForSnapshotHashIsDeterministic(t *testing.T) {
	in := SnapshotInput{
		Summary: map[string]int{"pods": 1},
		Groups: []SnapshotGroup{{Kind: "pods", Items: []SnapshotItem{{
			Kind: "Pod", Name: "api", Labels: map[string]string{"b": "2", "a": "1"},
		}}}},
	}
	a, err := ForSnapshot(in)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ForSnapshot(in)
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

func TestForSnapshotEmptyIsHarmless(t *testing.T) {
	p, err := ForSnapshot(SnapshotInput{Summary: map[string]int{}})
	if err != nil {
		t.Fatal(err)
	}
	if p.Envelope.Truncated {
		t.Error("an empty snapshot is not truncated")
	}
}
