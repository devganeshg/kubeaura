package ai

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// Assistant is the AI assistant. It is model-agnostic and holds a runtime registry
// of model Connections (Anthropic, Ollama, or OpenAI-compatible). Operators can
// add, activate, and remove connections from the UI without a restart. It is
// safe for concurrent use. When no connection is active the Assistant is disabled.
type Assistant struct {
	mu        sync.RWMutex
	order     []string               // connection ids, in insertion order
	conns     map[string]*Connection // id -> connection (includes secrets)
	providers map[string]Provider    // id -> live provider
	active    string                 // active connection id ("" = disabled)
}

// Settings selects and configures the initial (env-derived) AI backend. Empty
// Provider means auto-detect; if nothing is configured the Assistant starts
// disabled but connections can still be added later from the UI.
type Settings struct {
	Provider      string
	Model         string
	AnthropicKey  string
	OllamaHost    string
	OpenAIBaseURL string
	OpenAIKey     string
}

// Configure builds a Assistant and seeds it with an env-derived connection when
// one can be inferred (see selection order in the docs). The Assistant is always
// returned non-nil so connections can be managed at runtime regardless.
func Configure(s Settings) *Assistant {
	c := &Assistant{conns: map[string]*Connection{}, providers: map[string]Provider{}}
	if conn, ok := envConnection(s); ok {
		// Best-effort: if the env connection can't build a provider we still
		// start (disabled) so the UI can fix it.
		_, _ = c.add(conn, true)
	}
	return c
}

// envConnection derives a Connection from Settings using the auto-detect rules:
// explicit provider wins, else Anthropic key, else OpenAI base/key, else Ollama.
func envConnection(s Settings) (Connection, bool) {
	p := strings.ToLower(strings.TrimSpace(s.Provider))
	mk := func(provider, baseURL, key string) Connection {
		return Connection{Name: "Startup (" + provider + ")", Provider: provider, BaseURL: baseURL, APIKey: key, Model: s.Model, Source: "env"}
	}
	switch p {
	case "anthropic", "claude":
		if s.AnthropicKey == "" {
			return Connection{}, false
		}
		return mk("anthropic", "", s.AnthropicKey), true
	case "ollama", "local":
		return mk("ollama", s.OllamaHost, ""), true
	case "openai", "openai-compatible", "localai", "lmstudio", "vllm", "custom":
		return mk("openai", s.OpenAIBaseURL, s.OpenAIKey), true
	case "":
		// auto-detect
	default:
		return Connection{}, false
	}
	switch {
	case s.AnthropicKey != "":
		return mk("anthropic", "", s.AnthropicKey), true
	case s.OpenAIBaseURL != "" || s.OpenAIKey != "":
		return mk("openai", s.OpenAIBaseURL, s.OpenAIKey), true
	case s.OllamaHost != "":
		return mk("ollama", s.OllamaHost, ""), true
	default:
		return Connection{}, false
	}
}

// New preserves the original Anthropic-only constructor.
func New(apiKey, model string) *Assistant {
	return Configure(Settings{Provider: "anthropic", AnthropicKey: apiKey, Model: model})
}

func providerLabel(baseURL string) string {
	if baseURL == "" || strings.Contains(baseURL, "api.openai.com") {
		return "OpenAI"
	}
	return "Custom (OpenAI-compatible)"
}

// --- connection registry API ---

// add validates and stores a connection, building its provider. When makeActive
// is true (or it's the first connection) it becomes the active backend.
func (c *Assistant) add(conn Connection, makeActive bool) (Connection, error) {
	prov, err := buildProvider(conn)
	if err != nil {
		return Connection{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if conn.ID == "" {
		conn.ID = newID()
	}
	if conn.Name == "" {
		conn.Name = prov.Name()
	}
	if conn.Model == "" {
		conn.Model = prov.Model()
	}
	c.conns[conn.ID] = &conn
	c.providers[conn.ID] = prov
	c.order = append(c.order, conn.ID)
	if makeActive || c.active == "" {
		c.active = conn.ID
	}
	return conn, nil
}

// AddConnection adds a user-defined connection from the UI and activates it.
func (c *Assistant) AddConnection(conn Connection) (Connection, error) {
	conn.Source = "user"
	saved, err := c.add(conn, true)
	if err != nil {
		return Connection{}, err
	}
	return saved.redacted(), nil
}

// Connections returns all connections (secrets redacted) with the active flag.
func (c *Assistant) Connections() ([]Connection, string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Connection, 0, len(c.order))
	for _, id := range c.order {
		out = append(out, c.conns[id].redacted())
	}
	return out, c.active
}

// Activate switches the active backend.
func (c *Assistant) Activate(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.conns[id]; !ok {
		return fmt.Errorf("no such connection %q", id)
	}
	c.active = id
	return nil
}

// RemoveConnection deletes a connection, clearing the active pointer if needed.
func (c *Assistant) RemoveConnection(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.conns[id]; !ok {
		return fmt.Errorf("no such connection %q", id)
	}
	delete(c.conns, id)
	delete(c.providers, id)
	for i, x := range c.order {
		if x == id {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
	if c.active == id {
		c.active = ""
		if len(c.order) > 0 {
			c.active = c.order[0]
		}
	}
	return nil
}

// TestConnection runs a tiny generation against a stored connection.
func (c *Assistant) TestConnection(ctx context.Context, id string) error {
	c.mu.RLock()
	prov := c.providers[id]
	c.mu.RUnlock()
	if prov == nil {
		return fmt.Errorf("no such connection %q", id)
	}
	return pingProvider(ctx, prov)
}

func (c *Assistant) activeProvider() Provider {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.active == "" {
		return nil
	}
	return c.providers[c.active]
}

// Enabled reports whether an active model backend is configured.
func (c *Assistant) Enabled() bool { return c.activeProvider() != nil }

// ProviderName returns the active backend's label, or "none".
func (c *Assistant) ProviderName() string {
	if p := c.activeProvider(); p != nil {
		return p.Name()
	}
	return "none"
}

// ModelName returns the active model id, or "".
func (c *Assistant) ModelName() string {
	if p := c.activeProvider(); p != nil {
		return p.Model()
	}
	return ""
}

// complete delegates to the active provider.
func (c *Assistant) complete(ctx context.Context, system, user string, maxTokens int, effort string) (string, error) {
	p := c.activeProvider()
	if p == nil {
		return "", fmt.Errorf("AI Assistant is disabled: add a model connection (Anthropic, Ollama, or an OpenAI-compatible server) to enable natural-language queries, troubleshooting, and manifest generation")
	}
	return p.Complete(ctx, system, user, maxTokens, effort)
}

// --- High-level capabilities ---

const querySystem = `You are KubeAura, the voice assistant inside a Kubernetes cluster management UI. You are given
a JSON snapshot of the user's live cluster (summary counts plus lists of pods, deployments, services, nodes, and
events) and an optional DOCUMENT CONTEXT block sourced from project documentation.

Answer style — facts first, always:
- Lead with the direct answer to the question: exact numbers and exact resource names copied from the snapshot
  (pod/deployment/service names, namespaces, nodes). Never answer vaguely when the snapshot has the specifics.
- Keep it to 1–3 short sentences; answers are spoken aloud. Never recite raw YAML, JSON, or full object dumps —
  the details are already on screen.
- If something is unhealthy (failed pods, not-ready nodes, warning events), name the exact resource and add the
  single most likely next step.
- A light touch of dry, polite wit (an occasional "sir") is welcome AFTER the facts, never instead of them.

Rules:
- The cluster snapshot is the only source of truth for runtime state. Do not invent resources that are not in it.
- If the snapshot does not contain the data needed, say exactly what is missing — do not guess.
- Use DOCUMENT CONTEXT for platform standards, policy, and recommended practices; cite as [source: <path>].
- If docs guidance and live cluster state conflict, call that out explicitly.`

// Query answers a natural-language question grounded in a cluster snapshot.
func (c *Assistant) Query(ctx context.Context, question, snapshotJSON string) (string, error) {
	return c.QueryWithDocs(ctx, question, snapshotJSON, "")
}

// QueryWithDocs answers using cluster snapshot plus optional docs context.
func (c *Assistant) QueryWithDocs(ctx context.Context, question, snapshotJSON, docsContext string) (string, error) {
	user := fmt.Sprintf("CLUSTER SNAPSHOT (JSON):\n%s\n\n%s\nQUESTION: %s", snapshotJSON, docsContext, question)
	return c.complete(ctx, querySystem, user, 2048, "medium")
}

// QueryStreamWithDocs is QueryWithDocs with incremental delivery: onDelta
// receives text chunks as the model produces them. When the active provider
// cannot stream, the full answer arrives as a single delta. The returned
// string is always the complete answer.
func (c *Assistant) QueryStreamWithDocs(ctx context.Context, question, snapshotJSON, docsContext string, onDelta func(string)) (string, error) {
	p := c.activeProvider()
	if p == nil {
		return "", fmt.Errorf("AI Assistant is disabled: add a model connection (Anthropic, Ollama, or an OpenAI-compatible server) to enable natural-language queries, troubleshooting, and manifest generation")
	}
	user := fmt.Sprintf("CLUSTER SNAPSHOT (JSON):\n%s\n\n%s\nQUESTION: %s", snapshotJSON, docsContext, question)
	if sp, ok := p.(StreamingProvider); ok {
		return sp.CompleteStream(ctx, querySystem, user, 2048, "medium", onDelta)
	}
	answer, err := p.Complete(ctx, querySystem, user, 2048, "medium")
	if err == nil && answer != "" {
		onDelta(answer)
	}
	return answer, err
}

const troubleshootSystem = `You are KubeAura, an expert Kubernetes troubleshooter. You are given the full JSON
description of a single pod, including its spec, status, recent events, and container logs. Produce a focused
Root Cause Analysis with these markdown sections:
## Root Cause
## Evidence (cite the specific events/log lines/status fields)
## Suggested Fix (concrete kubectl commands or YAML changes)
## Prevention (best practice)
Be direct. If the pod appears healthy, say so.`

// Troubleshoot performs root-cause analysis on a pod's detail bundle.
func (c *Assistant) Troubleshoot(ctx context.Context, podDetailJSON string) (string, error) {
	user := fmt.Sprintf("POD DETAIL (JSON):\n%s\n\nDiagnose this pod.", podDetailJSON)
	return c.complete(ctx, troubleshootSystem, user, 3072, "high")
}

const triageSystem = `You are KubeAura, an SRE on-call triage assistant. You are given a JSON list of the
cluster's current alerts (crashlooping pods, degraded workloads, node pressure, failed jobs, warning events, etc.)
plus a health summary. Produce a prioritized incident triage using this markdown structure:
## Top Priority (what to fix first, and why it matters most)
## Likely Root Causes (group related alerts — e.g. one failing image causing many pod alerts)
## Action Plan (numbered, concrete kubectl commands or YAML changes)
## Noise (alerts that are low-priority or expected, so the operator can ignore them)
Be concise and specific: reference the exact namespaces, pods, and workloads. If there are no alerts, say the
cluster is healthy and suggest one proactive check.`

// Triage turns the live alert report into a prioritized, explained action plan.
func (c *Assistant) Triage(ctx context.Context, alertsJSON string) (string, error) {
	user := fmt.Sprintf("CLUSTER ALERTS (JSON):\n%s\n\nTriage these for me.", alertsJSON)
	return c.complete(ctx, triageSystem, user, 3072, "high")
}

const reviewSystem = `You are KubeAura, a Kubernetes manifest reviewer. You are given a YAML manifest. Review it
and respond in markdown with these sections:
## Summary (one line: what this manifest deploys)
## Correctness (bugs, invalid fields, missing required settings)
## Security (runAsNonRoot, readOnlyRootFilesystem, dropped capabilities, no privileged, secrets handling)
## Reliability (resource requests/limits, liveness/readiness probes, replicas, PDBs, anti-affinity)
## Suggested Changes (a concise unified diff or the specific YAML snippets to change)
Be specific and cite the exact keys. If the manifest is already solid, say so and note only genuine improvements.`

// Review analyzes a Kubernetes manifest for correctness, security, and reliability.
func (c *Assistant) Review(ctx context.Context, yaml string) (string, error) {
	user := fmt.Sprintf("MANIFEST (YAML):\n%s\n\nReview this manifest.", yaml)
	return c.complete(ctx, reviewSystem, user, 3072, "high")
}

const topologySystem = `You are KubeAura, a Kubernetes architecture explainer. You are given a JSON graph of a
namespace's resources: nodes (Ingress, Service, Deployment/StatefulSet/DaemonSet, Pod) and edges (routes, selects,
owns). Explain the architecture in plain language with this markdown structure:
## Traffic Path (how a request flows from Ingress down to Pods)
## Workloads (what each Deployment/StatefulSet runs and its pods)
## Observations (anything notable: a Service selecting zero pods, an unhealthy pod in the path, no Ingress, single
replica, etc.)
Be concise and reference exact resource names. Do not invent resources not in the graph.`

// ExplainTopology describes a namespace's resource graph in plain language.
func (c *Assistant) ExplainTopology(ctx context.Context, topologyJSON string) (string, error) {
	user := fmt.Sprintf("NAMESPACE TOPOLOGY (JSON):\n%s\n\nExplain this architecture.", topologyJSON)
	return c.complete(ctx, topologySystem, user, 2048, "medium")
}

const logSummarySystem = `You are KubeAura. You are given raw container log lines from a Kubernetes pod. Summarize
them for an operator in markdown:
## Summary (what the app is doing / its state in 1-2 lines)
## Errors & Warnings (the distinct problems, most severe first, with the exact log line)
## Likely Cause & Next Step (only if errors are present)
If the logs look healthy, say so. Be concise; collapse repeated lines.`

// SummarizeLogs distills raw log output into a short operator summary.
func (c *Assistant) SummarizeLogs(ctx context.Context, logs string) (string, error) {
	user := fmt.Sprintf("CONTAINER LOGS:\n%s\n\nSummarize these logs.", logs)
	return c.complete(ctx, logSummarySystem, user, 2048, "medium")
}

const generateSystem = `You are KubeAura, a Kubernetes manifest generator. The user describes a workload in
natural language. Output ONLY valid, apply-ready Kubernetes YAML — no prose, no markdown fences, no explanation.
Follow best practices: set resource requests/limits, liveness/readiness probes where sensible, labels, and a
non-root securityContext. Use apiVersion apps/v1 for Deployments. If multiple objects are needed (e.g. Deployment
plus Service), separate them with '---'.`

// Generate produces Kubernetes YAML from a natural-language description.
func (c *Assistant) Generate(ctx context.Context, description string) (string, error) {
	out, err := c.complete(ctx, generateSystem, description, 3072, "medium")
	if err != nil {
		return "", err
	}
	out = strings.TrimSpace(out)
	out = strings.TrimPrefix(out, "```yaml")
	out = strings.TrimPrefix(out, "```")
	out = strings.TrimSuffix(out, "```")
	return strings.TrimSpace(out), nil
}
