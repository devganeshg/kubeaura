package k8s

import (
	"encoding/json"
	"strings"
	"testing"
)

// The proxy path needs a cluster; the parts that carry risk — decoding
// Prometheus' response shape and building label matchers from untrusted input
// — do not, and are covered here.

func TestQuoteLabelEscapesInjection(t *testing.T) {
	// A namespace name arriving from a query string must not be able to close
	// the matcher and append a selector of its own.
	got := matchers(`default",container!="x`, "pod", "")
	if strings.Contains(got, `namespace="default",container!="x"`) {
		t.Errorf("injection escaped the string literal: %s", got)
	}
	if !strings.Contains(got, `\"`) {
		t.Errorf("expected the embedded quote to be escaped: %s", got)
	}
}

func TestMatchersOmitEmptyValues(t *testing.T) {
	if got := matchers("payments", "pod", ""); got != `namespace="payments"` {
		t.Errorf("matchers = %q", got)
	}
	if got := matchers("", "pod", "api-7d9"); got != `pod="api-7d9"` {
		t.Errorf("matchers = %q", got)
	}
	// An unfiltered selector still has to be valid PromQL.
	if got := matchers("", "pod", ""); got != `namespace!=""` {
		t.Errorf("matchers = %q", got)
	}
}

func TestDecodeSample(t *testing.T) {
	var pair []json.RawMessage
	if err := json.Unmarshal([]byte(`[1754654400,"0.42"]`), &pair); err != nil {
		t.Fatal(err)
	}
	p, ok := decodeSample(pair)
	if !ok || p.Value != 0.42 {
		t.Fatalf("decodeSample = %+v ok=%v", p, ok)
	}
	if p.At.Unix() != 1754654400 {
		t.Errorf("timestamp = %d", p.At.Unix())
	}
}

func TestDecodeSampleDropsGapsRatherThanFailing(t *testing.T) {
	// Prometheus writes NaN for a gap. Losing the point is correct; losing the
	// surrounding hour of data is not — and encoding/json refuses to marshal a
	// NaN at all, so one would fail the entire response.
	var pair []json.RawMessage
	if err := json.Unmarshal([]byte(`[1754654400,"NaN"]`), &pair); err != nil {
		t.Fatal(err)
	}
	if _, ok := decodeSample(pair); ok {
		t.Error("NaN should not decode to a point")
	}
}

func TestSeriesNamePrefersTheIdentifyingLabel(t *testing.T) {
	if got := seriesName(map[string]string{"pod": "api-7d9", "namespace": "payments"}); got != "api-7d9" {
		t.Errorf("seriesName = %q, want api-7d9", got)
	}
	if got := seriesName(map[string]string{"namespace": "payments"}); got != "payments" {
		t.Errorf("seriesName = %q", got)
	}
	// No known label: fall back to something stable rather than empty.
	got := seriesName(map[string]string{"b": "2", "a": "1"})
	if got != "a=1,b=2" {
		t.Errorf("seriesName = %q, want a stable joined form", got)
	}
}

func TestPromEnvelopeDecodesRangeAndInstant(t *testing.T) {
	const body = `{"status":"success","data":{"resultType":"matrix","result":[
	  {"metric":{"pod":"api"},"values":[[1754654400,"1"],[1754654460,"2"]]}]}}`
	var env promEnvelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Data.Result) != 1 || len(env.Data.Result[0].Values) != 2 {
		t.Fatalf("decoded %+v", env.Data.Result)
	}
	if env.Data.Result[0].Metric["pod"] != "api" {
		t.Errorf("labels lost: %+v", env.Data.Result[0].Metric)
	}
}

func TestPromHistoryRejectsUnknownKind(t *testing.T) {
	c := &Client{}
	if _, err := c.PromHistory(nil, "not-a-kind", "", "", 0); err == nil {
		t.Fatal("expected an error for an unknown history kind")
	}
}
