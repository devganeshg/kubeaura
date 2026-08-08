// Package alertstate gives KubeAura's alerts a memory.
//
// Rule evaluation in internal/k8s is stateless by design: it reads live cluster
// state and derives what is wrong right now. That answers "what is broken" but
// not the two questions an operator actually asks during an incident — "when
// did this start" and "is this the thing I already looked at". A list of 200
// warnings with no answer to either is a wall, not a signal.
//
// The Tracker sits between evaluation and the API and remembers alerts by
// fingerprint across evaluations: when each was first seen, how long it has
// been firing, how many evaluations it survived, whether it has been
// acknowledged, and what stopped firing since last time.
//
// State is in memory and per-process, matching the rest of KubeAura: it is an
// operator tool, not a monitoring system. Restarting the binary forgets
// acknowledgements, which is the right trade — a stale ack that outlives the
// incident is worse than re-acking.
package alertstate

import (
	"sort"
	"sync"
	"time"

	"github.com/devganeshg/kubeaura/internal/k8s"
)

const (
	// newAlertWindow is how long an alert is flagged as new. Long enough to
	// survive a dashboard refresh, short enough that "new" still means new.
	newAlertWindow = 5 * time.Minute

	// resolvedRetention is how long a resolved alert stays in the report, so a
	// fix is visible on the next look rather than vanishing silently.
	resolvedRetention = 30 * time.Minute

	// forgetAfter drops an entry that has neither fired nor resolved recently,
	// bounding memory on a cluster that churns through alerts.
	forgetAfter = 2 * time.Hour
)

// Tracker remembers alert state across evaluations, keyed by cluster context so
// switching contexts does not report one cluster's history against another.
type Tracker struct {
	mu     sync.Mutex
	byCtx  map[string]map[string]*entry
	silent map[string]silence

	// now is injectable so the tests can advance time without sleeping.
	now func() time.Time
}

type entry struct {
	alert       k8s.Alert
	firstSeen   time.Time
	lastSeen    time.Time
	occurrences int
	resolvedAt  time.Time // zero while firing
}

type silence struct {
	note  string
	until time.Time // zero means until the alert resolves
}

// New returns an empty Tracker.
func New() *Tracker {
	return &Tracker{
		byCtx:  map[string]map[string]*entry{},
		silent: map[string]silence{},
		now:    time.Now,
	}
}

// Observe records one evaluation and returns the report enriched with temporal
// state. It mutates rep in place and returns it for convenience.
//
// The scope argument matters: a namespace-scoped evaluation must not conclude
// that every alert in other namespaces has resolved. Callers pass the namespace
// they asked for ("" for the whole cluster) and only alerts in that scope are
// considered for resolution.
func (t *Tracker) Observe(cluster, scope string, rep *k8s.AlertReport) *k8s.AlertReport {
	if rep == nil {
		return rep
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()

	seen := t.byCtx[cluster]
	if seen == nil {
		seen = map[string]*entry{}
		t.byCtx[cluster] = seen
	}

	firing := make(map[string]bool, len(rep.Alerts))
	for i := range rep.Alerts {
		a := &rep.Alerts[i]
		firing[a.Fingerprint] = true

		e := seen[a.Fingerprint]
		if e == nil || !e.resolvedAt.IsZero() {
			// Either new, or firing again after resolving. A recurrence is a
			// fresh incident: its clock restarts and any acknowledgement from
			// the previous round is dropped, because "I looked at this" was
			// about a problem that then went away.
			e = &entry{firstSeen: now}
			seen[a.Fingerprint] = e
			delete(t.silent, key(cluster, a.Fingerprint))
		}
		e.alert = *a
		e.lastSeen = now
		e.occurrences++
		e.resolvedAt = time.Time{}

		a.FirstSeen = e.firstSeen
		a.LastSeen = now
		a.Occurrences = e.occurrences
		a.ActiveFor = shortDuration(now.Sub(e.firstSeen))
		a.New = now.Sub(e.firstSeen) < newAlertWindow

		if s, ok := t.silent[key(cluster, a.Fingerprint)]; ok {
			if s.until.IsZero() || now.Before(s.until) {
				a.Acked, a.AckNote = true, s.note
			} else {
				delete(t.silent, key(cluster, a.Fingerprint))
			}
		}
	}

	// Anything previously firing in this scope that is absent now has resolved.
	for fp, e := range seen {
		if firing[fp] || !e.resolvedAt.IsZero() {
			continue
		}
		if !inScope(e.alert, scope) {
			continue
		}
		e.resolvedAt = now
		delete(t.silent, key(cluster, fp))
	}

	rep.Resolved = nil
	for fp, e := range seen {
		switch {
		case e.resolvedAt.IsZero():
			continue
		case now.Sub(e.resolvedAt) > forgetAfter:
			delete(seen, fp)
		case now.Sub(e.resolvedAt) <= resolvedRetention:
			rep.Resolved = append(rep.Resolved, k8s.Resolved{
				Fingerprint: fp,
				Severity:    e.alert.Severity,
				Kind:        e.alert.Kind,
				Namespace:   e.alert.Namespace,
				Name:        e.alert.Name,
				Title:       e.alert.Title,
				FirstSeen:   e.firstSeen,
				ResolvedAt:  e.resolvedAt,
				Lasted:      shortDuration(e.resolvedAt.Sub(e.firstSeen)),
			})
		}
	}
	sort.Slice(rep.Resolved, func(i, j int) bool {
		return rep.Resolved[i].ResolvedAt.After(rep.Resolved[j].ResolvedAt)
	})

	rep.Acked, rep.New = 0, 0
	for _, a := range rep.Alerts {
		if a.Acked {
			rep.Acked++
		}
		if a.New {
			rep.New++
		}
	}

	// Within a severity band, newest first. An alert that started two minutes
	// ago is the one worth looking at; the disk-pressure warning that has been
	// firing for three days is not what just broke.
	rank := map[string]int{"critical": 0, "warning": 1, "info": 2}
	sort.SliceStable(rep.Alerts, func(i, j int) bool {
		a, b := rep.Alerts[i], rep.Alerts[j]
		// Acknowledged alerts sink: triaging one is how you get it out of the
		// way, so it must actually get out of the way.
		if a.Acked != b.Acked {
			return !a.Acked
		}
		if rank[a.Severity] != rank[b.Severity] {
			return rank[a.Severity] < rank[b.Severity]
		}
		return a.FirstSeen.After(b.FirstSeen)
	})
	return rep
}

// Ack marks an alert as triaged. A zero until means "until it resolves", which
// is the common case: you looked at it, you know, stop showing it to you until
// something actually changes.
func (t *Tracker) Ack(cluster, fingerprint, note string, until time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.silent[key(cluster, fingerprint)] = silence{note: note, until: until}
}

// Unack removes an acknowledgement.
func (t *Tracker) Unack(cluster, fingerprint string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.silent, key(cluster, fingerprint))
}

// FirstSeen reports when a fingerprint started firing, for callers correlating
// an alert against cluster changes. The second return is false when the alert
// is unknown to the tracker.
func (t *Tracker) FirstSeen(cluster, fingerprint string) (time.Time, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.byCtx[cluster][fingerprint]
	if !ok {
		return time.Time{}, false
	}
	return e.firstSeen, true
}

func key(cluster, fingerprint string) string { return cluster + "|" + fingerprint }

// inScope reports whether an alert belongs to the namespace just evaluated.
// Cluster-scoped alerts (nodes) have no namespace and are only in scope for a
// whole-cluster evaluation.
func inScope(a k8s.Alert, scope string) bool {
	if scope == "" {
		return true
	}
	return a.Namespace == scope
}

// shortDuration renders a duration the way an operator reads one: the largest
// useful unit, no decimals, no "0h0m12.4s".
func shortDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return itoa(int(d.Minutes())) + "m"
	case d < 24*time.Hour:
		return itoa(int(d.Hours())) + "h"
	default:
		return itoa(int(d.Hours()/24)) + "d"
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
