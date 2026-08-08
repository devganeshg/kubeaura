package k8s

import (
	"testing"
	"time"
)

// The per-source readers need a cluster; CorrelateChanges is the part that
// carries the judgement, so it is the part pinned down here.

func at(min int) time.Time {
	return time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC).Add(time.Duration(min) * time.Minute)
}

func TestCorrelateAttachesChangesJustBeforeTheAlert(t *testing.T) {
	alerts := []Alert{{
		Namespace: "payments", Name: "api-7d9", Title: "CrashLoopBackOff",
		FirstSeen: at(30),
	}}
	changes := []Change{
		{At: at(28), Source: "helm", Namespace: "payments", Name: "api", Summary: "api → revision 7"},
		{At: at(10), Source: "helm", Namespace: "payments", Name: "api", Summary: "api → revision 6"},
	}
	CorrelateChanges(alerts, changes, 15*time.Minute)

	if len(alerts[0].Suspects) != 1 {
		t.Fatalf("expected 1 suspect, got %+v", alerts[0].Suspects)
	}
	if alerts[0].Suspects[0].Summary != "api → revision 7" {
		t.Errorf("wrong suspect: %+v", alerts[0].Suspects[0])
	}
}

func TestCorrelateIgnoresChangesAfterTheAlertStarted(t *testing.T) {
	// A deploy that happened after the alert began cannot have caused it —
	// most often it is the fix, and offering it as a suspect is worse than
	// offering nothing.
	alerts := []Alert{{Namespace: "payments", Name: "api", FirstSeen: at(30)}}
	changes := []Change{{At: at(35), Source: "helm", Namespace: "payments", Name: "api"}}
	CorrelateChanges(alerts, changes, 15*time.Minute)
	if len(alerts[0].Suspects) != 0 {
		t.Errorf("a later change should not be a suspect: %+v", alerts[0].Suspects)
	}
}

func TestCorrelateRespectsTheWindow(t *testing.T) {
	alerts := []Alert{{Namespace: "payments", Name: "api", FirstSeen: at(60)}}
	changes := []Change{{At: at(10), Source: "helm", Namespace: "payments", Name: "api"}}
	CorrelateChanges(alerts, changes, 15*time.Minute)
	if len(alerts[0].Suspects) != 0 {
		t.Errorf("a change 50m earlier is outside a 15m window: %+v", alerts[0].Suspects)
	}
}

func TestCorrelateStaysInNamespaceButClusterChangesApplyEverywhere(t *testing.T) {
	alerts := []Alert{{Namespace: "payments", Name: "api", FirstSeen: at(30)}}
	changes := []Change{
		{At: at(25), Source: "helm", Namespace: "shop", Name: "web"},
		{At: at(26), Source: "node", Name: "ip-10-0-3-14"}, // cluster-scoped
	}
	CorrelateChanges(alerts, changes, 15*time.Minute)
	if len(alerts[0].Suspects) != 1 {
		t.Fatalf("expected only the cluster-scoped change, got %+v", alerts[0].Suspects)
	}
	if alerts[0].Suspects[0].Source != "node" {
		t.Errorf("wrong suspect: %+v", alerts[0].Suspects[0])
	}
}

func TestCorrelateSkipsUntrackedAlerts(t *testing.T) {
	// Without a FirstSeen there is no moment to correlate against, and dating
	// the alert from "now" would make every recent change a suspect.
	alerts := []Alert{{Namespace: "payments", Name: "api"}}
	changes := []Change{{At: at(25), Source: "helm", Namespace: "payments", Name: "api"}}
	CorrelateChanges(alerts, changes, 15*time.Minute)
	if len(alerts[0].Suspects) != 0 {
		t.Errorf("untracked alert should get no suspects: %+v", alerts[0].Suspects)
	}
}

func TestCorrelateCapsAndOrdersSuspects(t *testing.T) {
	alerts := []Alert{{Namespace: "payments", Name: "api", FirstSeen: at(60)}}
	var changes []Change
	for i := 1; i <= 6; i++ {
		changes = append(changes, Change{
			At: at(60 - i), Source: "rollout", Namespace: "payments", Name: "api", Revision: i,
		})
	}
	CorrelateChanges(alerts, changes, 15*time.Minute)
	if len(alerts[0].Suspects) != 3 {
		t.Fatalf("suspects should cap at 3, got %d", len(alerts[0].Suspects))
	}
	// Closest to the alert first: that is the one worth looking at.
	if alerts[0].Suspects[0].Revision != 1 {
		t.Errorf("suspects should be newest-first, got %+v", alerts[0].Suspects)
	}
}

func TestShortSince(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "just now"},
		{12 * time.Minute, "12m ago"},
		{3 * time.Hour, "3h ago"},
		{50 * time.Hour, "2d ago"},
	} {
		if got := shortSince(tc.d); got != tc.want {
			t.Errorf("shortSince(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestCorrelateSkipsAdvisoryAlerts(t *testing.T) {
	// An info alert is standing advice, not an incident: attaching the deploy
	// that preceded it implies a causal link that is not there.
	alerts := []Alert{{
		Severity: "info", Namespace: "payments", Name: "api",
		Title: "No resource requests", FirstSeen: at(30),
	}}
	changes := []Change{{At: at(25), Source: "rollout", Namespace: "payments", Name: "api"}}
	CorrelateChanges(alerts, changes, 15*time.Minute)
	if len(alerts[0].Suspects) != 0 {
		t.Errorf("info alerts should get no suspects: %+v", alerts[0].Suspects)
	}
}

func TestCorrelatePrefersChangesToTheSameWorkload(t *testing.T) {
	// A busy namespace deploys all day. If a rollout of an unrelated service
	// ranks alongside the rollout of the thing that is actually broken, the
	// answer is buried.
	alerts := []Alert{{
		Severity: "critical", Namespace: "payments", Name: "api-7d98b64fc9-x2k1p",
		Title: "CrashLoopBackOff", FirstSeen: at(30),
	}}
	changes := []Change{
		{At: at(29), Source: "rollout", Namespace: "payments", Name: "unrelated-worker"},
		{At: at(28), Source: "rollout", Namespace: "payments", Name: "api", Summary: "api → revision 7"},
	}
	CorrelateChanges(alerts, changes, 15*time.Minute)

	if len(alerts[0].Suspects) != 1 {
		t.Fatalf("expected only the matching workload, got %+v", alerts[0].Suspects)
	}
	if alerts[0].Suspects[0].Name != "api" {
		t.Errorf("wrong suspect: %+v", alerts[0].Suspects[0])
	}
}

func TestCorrelateFallsBackToTheNamespace(t *testing.T) {
	// Nothing touched this object, so a neighbouring change is the only lead
	// there is — worth showing, unlike when a direct one exists.
	alerts := []Alert{{
		Severity: "critical", Namespace: "payments", Name: "api-7d98b64fc9-x2k1p",
		FirstSeen: at(30),
	}}
	changes := []Change{{At: at(28), Source: "node", Name: "ip-10-0-3-14"}}
	CorrelateChanges(alerts, changes, 15*time.Minute)
	if len(alerts[0].Suspects) != 1 {
		t.Errorf("expected the cluster-scoped change as a fallback: %+v", alerts[0].Suspects)
	}
}

func TestSameWorkloadRequiresANameBoundary(t *testing.T) {
	if !sameWorkload("api-7d98b64fc9-x2k1p", "api") {
		t.Error("a pod should match its deployment")
	}
	if !sameWorkload("api", "api") {
		t.Error("an exact name should match")
	}
	if !sameWorkload("canary-crash-57985ffcc7-xh2wz", "canary-crash") {
		t.Error("a real pod name should match its deployment")
	}
	if !sameWorkload("kube-proxy-x7z9k", "kube-proxy") {
		t.Error("a DaemonSet pod has only the five-character suffix")
	}
	// The case a bare prefix test gets wrong: calling a gateway rollout the
	// cause of an API outage is worse than offering nothing.
	if sameWorkload("api-gateway-7d98b64fc9-x2k1p", "api") {
		t.Error("api-gateway is not api")
	}
	if sameWorkload("api-gateway", "api") {
		t.Error("api-gateway is not api")
	}
	if sameWorkload("api", "") || sameWorkload("", "api") {
		t.Error("an empty name should never match")
	}
}
