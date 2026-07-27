package report

import (
	"encoding/csv"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func cleanReport() *Report {
	r := &Report{
		Version:    "v1.2.3",
		Context:    "prod",
		ClusterVer: "v1.31.2",
		Thresholds: DefaultThresholds(),
		Vulns:      VulnSection{Scanner: "Trivy Operator", Installed: true, Reports: 4},
		Policy:     PolicySection{Engine: "Kyverno", Installed: true, Reports: 3, Pass: 30},
		RBAC:       RBACSection{Checked: true, ServiceAccount: "default", IsClusterAdmin: true},
	}
	r.Finalize()
	return r
}

func TestFinalizePassesWhenEveryCheckRanAndFoundNothing(t *testing.T) {
	r := cleanReport()
	if !r.Compliant {
		t.Errorf("want compliant, got violations %v skipped %v", r.Violations, r.Skipped)
	}
	if len(r.Skipped) != 0 {
		t.Errorf("nothing should be skipped: %v", r.Skipped)
	}
}

func TestFinalizeFlagsThresholdBreaches(t *testing.T) {
	r := cleanReport()
	r.Vulns.Critical = 3
	r.Vulns.High = 7
	r.Policy.Fail = 2
	r.Finalize()

	if r.Compliant {
		t.Fatal("report with critical CVEs and failing policies must not pass")
	}
	joined := strings.Join(r.Violations, "\n")
	for _, want := range []string{"critical image CVEs: 3", "high image CVEs: 7", "failing policy rules: 2"} {
		if !strings.Contains(joined, want) {
			t.Errorf("violations missing %q:\n%s", want, joined)
		}
	}
}

func TestFinalizeHonoursRelaxedThresholds(t *testing.T) {
	r := cleanReport()
	r.Vulns.High = 4
	r.Thresholds.MaxHigh = 5
	r.Finalize()

	if !r.Compliant {
		t.Errorf("4 high CVEs under a bar of 5 should pass, got %v", r.Violations)
	}
}

// A check that could not run must never read as a pass. This is the one
// failure mode of a compliance export that would actively mislead someone.
func TestFinalizeDoesNotPassWhenChecksWereSkipped(t *testing.T) {
	r := &Report{Thresholds: DefaultThresholds()}
	r.Finalize()

	if r.Compliant {
		t.Fatal("a report where no check ran must not be reported compliant")
	}
	if len(r.Skipped) != 3 {
		t.Errorf("want all three checks recorded as skipped, got %v", r.Skipped)
	}
	joined := strings.Join(r.Skipped, "\n")
	for _, want := range []string{"vulnerability", "policy", "RBAC"} {
		if !strings.Contains(joined, want) {
			t.Errorf("skipped list does not mention %q:\n%s", want, joined)
		}
	}
}

// Finalize is called twice by the handler path (once per gather pass in tests);
// it must not accumulate duplicate violations.
func TestFinalizeIsIdempotentForViolations(t *testing.T) {
	r := cleanReport()
	r.Vulns.Critical = 1
	r.Finalize()
	first := len(r.Violations)
	r.Finalize()
	if len(r.Violations) != first {
		t.Errorf("violations grew from %d to %d on a second Finalize", first, len(r.Violations))
	}

	// Skipped is rebuilt too, or a re-finalized report lists every absent
	// check twice.
	empty := &Report{Thresholds: DefaultThresholds()}
	empty.Finalize()
	n := len(empty.Skipped)
	empty.Finalize()
	if len(empty.Skipped) != n {
		t.Errorf("skipped list grew from %d to %d on a second Finalize", n, len(empty.Skipped))
	}
}

func TestFinalizeSortsAndCapsWorstOffenders(t *testing.T) {
	r := cleanReport()
	for i := 0; i < 40; i++ {
		r.Vulns.Worst = append(r.Vulns.Worst, VulnRow{Workload: "w", Critical: int64(i % 7)})
	}
	r.Vulns.Worst = append(r.Vulns.Worst, VulnRow{Workload: "worst", Critical: 99})
	r.Finalize()

	if len(r.Vulns.Worst) != maxRows {
		t.Errorf("worst list has %d rows, want it capped at %d", len(r.Vulns.Worst), maxRows)
	}
	if r.Vulns.Worst[0].Workload != "worst" {
		t.Errorf("worst offender is not first: %+v", r.Vulns.Worst[0])
	}
}

func TestRenderJSONRoundTrips(t *testing.T) {
	r := cleanReport()
	r.Vulns.Critical = 2
	r.Finalize()

	out, err := r.Render("json")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var back Report
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if back.Compliant != r.Compliant || len(back.Violations) != len(r.Violations) {
		t.Errorf("round trip lost the verdict: %+v", back)
	}
}

func TestRenderCSVIsParseable(t *testing.T) {
	r := cleanReport()
	r.Vulns.Worst = []VulnRow{{Namespace: "web", Workload: "Deployment/api", Image: "nginx:1.25", Critical: 2}}
	r.Policy.Failures = []PolicyRow{{Namespace: "web", Policy: "require-limits", Rule: "check", Result: "fail", Message: "no limits, set"}}
	r.Finalize()

	out, err := r.Render("csv")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	rows, err := csv.NewReader(strings.NewReader(string(out))).ReadAll()
	if err != nil {
		t.Fatalf("rendered CSV does not parse: %v", err)
	}
	if len(rows) < 5 {
		t.Fatalf("CSV has only %d rows", len(rows))
	}
	if rows[0][0] != "section" {
		t.Errorf("missing header row: %v", rows[0])
	}
	// A comma inside a message must not shift the columns.
	for _, row := range rows {
		if len(row) != len(rows[0]) {
			t.Fatalf("ragged row %v against header %v", row, rows[0])
		}
	}
}

func TestRenderMarkdownEscapesTableCells(t *testing.T) {
	r := cleanReport()
	r.Policy.Failures = []PolicyRow{{Policy: "p", Rule: "r", Message: "a | b broke the table"}}
	r.Policy.Fail = 1
	r.Finalize()

	out := string(mustRender(t, r, "md"))
	if strings.Contains(out, "a | b") {
		t.Error("a pipe in a message was not escaped, so the table will break")
	}
	if !strings.Contains(out, `a \| b`) {
		t.Errorf("escaped cell missing from output:\n%s", out)
	}
}

// The HTML report renders strings that came from the cluster — resource names,
// policy messages. They must not be able to inject markup.
func TestRenderHTMLEscapesClusterStrings(t *testing.T) {
	r := cleanReport()
	r.Policy.Fail = 1
	r.Policy.Failures = []PolicyRow{{
		Namespace: "web",
		Policy:    "p",
		Rule:      "r",
		Resource:  `<img src=x onerror="alert(1)">`,
		Message:   `</td></script><script>alert(2)</script>`,
	}}
	r.Finalize()

	out := string(mustRender(t, r, "html"))
	if strings.Contains(out, "<script>alert(2)") || strings.Contains(out, `onerror="alert(1)"`) {
		t.Fatalf("cluster-controlled string was not escaped into the HTML report")
	}
	if !strings.Contains(out, "&lt;script&gt;") {
		t.Errorf("expected escaped markup in the output")
	}
	// The page must not depend on anything it cannot carry with it.
	for _, external := range []string{"http://", "https://", "//cdn"} {
		if strings.Contains(out, external) {
			t.Errorf("HTML report references an external resource (%q); it must be self-contained", external)
		}
	}
}

func TestRenderRejectsUnknownFormat(t *testing.T) {
	if _, err := cleanReport().Render("pdf"); err == nil {
		t.Fatal("want an error for an unsupported format")
	}
}

func TestFilenameIsSafeAndDated(t *testing.T) {
	r := cleanReport()
	r.GeneratedAt = time.Date(2026, 7, 27, 9, 30, 0, 0, time.UTC)
	r.Context = "arn:aws:eks:eu-west-1:1234/prod cluster"

	name := r.Filename("csv")
	if strings.ContainsAny(name, "/: ") {
		t.Errorf("filename carries path or shell-awkward characters: %q", name)
	}
	if !strings.HasSuffix(name, "-20260727-093000.csv") {
		t.Errorf("filename is not stamped with the generation time: %q", name)
	}
}

func TestContentTypes(t *testing.T) {
	cases := map[string]string{
		"json": "application/json",
		"csv":  "text/csv; charset=utf-8",
		"md":   "text/markdown; charset=utf-8",
		"html": "text/html; charset=utf-8",
	}
	for format, want := range cases {
		if got := ContentType(format); got != want {
			t.Errorf("ContentType(%q) = %q, want %q", format, got, want)
		}
	}
}

func mustRender(t *testing.T, r *Report, format string) []byte {
	t.Helper()
	out, err := r.Render(format)
	if err != nil {
		t.Fatalf("Render(%q): %v", format, err)
	}
	return out
}
