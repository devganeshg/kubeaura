package alertstate

import (
	"testing"
	"time"

	"github.com/devganeshg/kubeaura/internal/k8s"
)

// The tracker's whole job is behaviour over time, so these drive a fake clock
// rather than sleeping.

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func testTracker() (*Tracker, *clock) {
	c := &clock{t: time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)}
	tr := New()
	tr.now = c.now
	return tr, c
}

func report(alerts ...k8s.Alert) *k8s.AlertReport {
	return &k8s.AlertReport{Alerts: alerts}
}

func alert(sev, ns, name, title string) k8s.Alert {
	a := k8s.Alert{Severity: sev, Category: "Workload", Kind: "Pod", Namespace: ns, Name: name, Title: title}
	// Mirror what k8s.Alerts stamps, without exporting the hash.
	a.Fingerprint = ns + "/" + name + "/" + title
	return a
}

func find(rep *k8s.AlertReport, fp string) *k8s.Alert {
	for i := range rep.Alerts {
		if rep.Alerts[i].Fingerprint == fp {
			return &rep.Alerts[i]
		}
	}
	return nil
}

func TestTracksHowLongAnAlertHasBeenFiring(t *testing.T) {
	tr, c := testTracker()
	a := alert("critical", "payments", "api-7d9", "CrashLoopBackOff")

	first := tr.Observe("prod", "", report(a))
	got := find(first, a.Fingerprint)
	if got.Occurrences != 1 || !got.New {
		t.Fatalf("first sighting: occurrences=%d new=%v", got.Occurrences, got.New)
	}
	if got.ActiveFor != "just now" {
		t.Errorf("activeFor = %q, want %q", got.ActiveFor, "just now")
	}

	c.add(12 * time.Minute)
	second := tr.Observe("prod", "", report(a))
	got = find(second, a.Fingerprint)
	if got.ActiveFor != "12m" {
		t.Errorf("activeFor = %q, want 12m", got.ActiveFor)
	}
	if got.Occurrences != 2 {
		t.Errorf("occurrences = %d, want 2", got.Occurrences)
	}
	if got.New {
		t.Error("an alert firing for 12m is not new")
	}
	if !got.FirstSeen.Equal(first.Alerts[0].FirstSeen) {
		t.Error("firstSeen must not move while the alert keeps firing")
	}
}

func TestSeverityChangeIsTheSameProblem(t *testing.T) {
	// A warning crossing a threshold into critical is the same problem getting
	// worse. If it were a new fingerprint, the clock would reset exactly when
	// the duration became most interesting.
	tr, c := testTracker()
	warn := alert("warning", "payments", "api-7d9", "High restart count")
	warn.Detail = "5 container restarts"

	tr.Observe("prod", "", report(warn))
	c.add(20 * time.Minute)

	worse := warn
	worse.Severity = "critical"
	worse.Detail = "22 container restarts"
	got := find(tr.Observe("prod", "", report(worse)), warn.Fingerprint)
	if got == nil {
		t.Fatal("alert lost across a severity change")
	}
	if got.ActiveFor != "20m" {
		t.Errorf("activeFor = %q, want 20m — the clock should not reset", got.ActiveFor)
	}
}

func TestResolvedAlertsAreReported(t *testing.T) {
	tr, c := testTracker()
	a := alert("critical", "payments", "api-7d9", "OOMKilled")

	tr.Observe("prod", "", report(a))
	c.add(9 * time.Minute)
	rep := tr.Observe("prod", "", report())

	if len(rep.Resolved) != 1 {
		t.Fatalf("expected 1 resolved alert, got %d", len(rep.Resolved))
	}
	r := rep.Resolved[0]
	if r.Title != "OOMKilled" || r.Lasted != "9m" {
		t.Errorf("resolved = %+v", r)
	}

	// It ages out rather than lingering forever.
	c.add(resolvedRetention + time.Minute)
	if rep := tr.Observe("prod", "", report()); len(rep.Resolved) != 0 {
		t.Errorf("resolved alert should have aged out, got %+v", rep.Resolved)
	}
}

func TestNamespaceScopeDoesNotResolveOtherNamespaces(t *testing.T) {
	// Looking at one namespace must not conclude that everything elsewhere is
	// fixed — the alert simply was not evaluated.
	tr, _ := testTracker()
	pay := alert("critical", "payments", "api", "CrashLoopBackOff")
	shop := alert("critical", "shop", "web", "CrashLoopBackOff")
	tr.Observe("prod", "", report(pay, shop))

	rep := tr.Observe("prod", "payments", report(pay))
	if len(rep.Resolved) != 0 {
		t.Errorf("a namespace-scoped evaluation resolved out-of-scope alerts: %+v", rep.Resolved)
	}

	// A whole-cluster evaluation without it, though, does resolve it.
	rep = tr.Observe("prod", "", report(pay))
	if len(rep.Resolved) != 1 || rep.Resolved[0].Namespace != "shop" {
		t.Errorf("cluster-scoped evaluation should resolve shop, got %+v", rep.Resolved)
	}
}

func TestAckSinksAlertAndCountsSeparately(t *testing.T) {
	tr, _ := testTracker()
	noisy := alert("critical", "payments", "known", "No resource requests")
	fresh := alert("critical", "payments", "api", "OOMKilled")

	tr.Observe("prod", "", report(noisy, fresh))
	tr.Ack("prod", noisy.Fingerprint, "known issue, ticket OPS-42", time.Time{})

	rep := tr.Observe("prod", "", report(noisy, fresh))
	if rep.Acked != 1 {
		t.Errorf("acked count = %d, want 1", rep.Acked)
	}
	got := find(rep, noisy.Fingerprint)
	if !got.Acked || got.AckNote != "known issue, ticket OPS-42" {
		t.Errorf("ack not applied: %+v", got)
	}
	// Triaging an alert has to actually move it out of the way.
	if rep.Alerts[len(rep.Alerts)-1].Fingerprint != noisy.Fingerprint {
		t.Errorf("acked alert should sort last, got order %v", order(rep))
	}
}

func TestAckExpiresAfterItsWindow(t *testing.T) {
	tr, c := testTracker()
	a := alert("warning", "payments", "api", "High restart count")
	tr.Observe("prod", "", report(a))
	tr.Ack("prod", a.Fingerprint, "snoozing during the deploy", c.t.Add(30*time.Minute))

	if got := find(tr.Observe("prod", "", report(a)), a.Fingerprint); !got.Acked {
		t.Fatal("ack should hold inside its window")
	}
	c.add(31 * time.Minute)
	if got := find(tr.Observe("prod", "", report(a)), a.Fingerprint); got.Acked {
		t.Error("ack should have expired")
	}
}

func TestRecurrenceClearsTheAckAndRestartsTheClock(t *testing.T) {
	// "I looked at this" was about a problem that then went away. When it comes
	// back it is a new incident and must be surfaced, not silently pre-triaged.
	tr, c := testTracker()
	a := alert("critical", "payments", "api", "OOMKilled")
	tr.Observe("prod", "", report(a))
	tr.Ack("prod", a.Fingerprint, "restarted it", time.Time{})

	c.add(5 * time.Minute)
	tr.Observe("prod", "", report()) // resolves
	c.add(5 * time.Minute)
	rep := tr.Observe("prod", "", report(a)) // fires again

	got := find(rep, a.Fingerprint)
	if got.Acked {
		t.Error("a recurrence must not inherit the previous acknowledgement")
	}
	if !got.New || got.ActiveFor != "just now" {
		t.Errorf("a recurrence is a fresh incident: new=%v activeFor=%q", got.New, got.ActiveFor)
	}
	if got.Occurrences != 1 {
		t.Errorf("occurrences = %d, want 1 after a recurrence", got.Occurrences)
	}
}

func TestNewestFirstWithinSeverity(t *testing.T) {
	tr, c := testTracker()
	old := alert("critical", "payments", "old", "Node NotReady")
	tr.Observe("prod", "", report(old))
	c.add(3 * time.Hour)
	recent := alert("critical", "payments", "new", "OOMKilled")
	warn := alert("warning", "payments", "w", "PVC not bound")

	rep := tr.Observe("prod", "", report(old, recent, warn))
	if rep.Alerts[0].Fingerprint != recent.Fingerprint {
		t.Errorf("what just broke should sort first, got %v", order(rep))
	}
	if rep.Alerts[2].Severity != "warning" {
		t.Errorf("severity must still outrank recency, got %v", order(rep))
	}
}

func TestClustersDoNotShareHistory(t *testing.T) {
	tr, c := testTracker()
	a := alert("critical", "payments", "api", "OOMKilled")
	tr.Observe("prod", "", report(a))
	c.add(2 * time.Hour)

	got := find(tr.Observe("staging", "", report(a)), a.Fingerprint)
	if !got.New {
		t.Error("staging should not inherit prod's history for the same fingerprint")
	}
}

func order(rep *k8s.AlertReport) []string {
	out := make([]string, 0, len(rep.Alerts))
	for _, a := range rep.Alerts {
		out = append(out, a.Fingerprint)
	}
	return out
}
