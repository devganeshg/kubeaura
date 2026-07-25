package api

import (
	"sync"
	"time"
)

// AuditEntry is one write action taken through KubeAura. The tool keeps a local
// trail of everything that mutates the cluster so an operator has a clear record
// of changes made outside CI/CD. It's in-memory (a ring buffer) — nothing is
// written to disk, matching the "never persist" posture.
type AuditEntry struct {
	Time    time.Time `json:"time"`
	Context string    `json:"context"`
	Action  string    `json:"action"` // apply, scale, restart, delete, port-forward, exec, …
	Target  string    `json:"target"` // kind/namespace/name or command
	Detail  string    `json:"detail"`
	Result  string    `json:"result"` // ok | error
}

// auditLog is a fixed-size ring buffer of recent write actions.
type auditLog struct {
	mu      sync.Mutex
	entries []AuditEntry
	max     int
}

func newAuditLog() *auditLog { return &auditLog{max: 500} }

func (a *auditLog) record(e AuditEntry) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, e)
	if len(a.entries) > a.max {
		a.entries = a.entries[len(a.entries)-a.max:]
	}
}

// list returns entries newest-first.
func (a *auditLog) list() []AuditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]AuditEntry, len(a.entries))
	for i, e := range a.entries {
		out[len(a.entries)-1-i] = e
	}
	return out
}
