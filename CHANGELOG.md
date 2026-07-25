# Changelog

All notable changes to KubeAura. The format follows
[Keep a Changelog](https://keepachangelog.com); versioning follows
[SemVer](https://semver.org). Pre-1.0, minor versions may break things — when
they do, it will say so here.

## [Unreleased]

## [v0.1.0] — 2026-07-25

First public release. KubeAura is a single binary that reads your existing
kubeconfig and serves a Kubernetes cockpit with an AI assistant you point at
whichever model you like — including one running entirely on your own machine.

Higher version numbers exist in the project's private history; nothing before
this was ever published, so the public record starts at v0.1.0.

### The cockpit

- **Command center** — cluster health, a live triage feed, pod-phase and
  deployment-health charts, an event timeline, and a restart leaderboard.
- **Alerts (Pulse)** — one prioritised list of everything wrong: crashloops,
  OOMKills, degraded workloads, node pressure, failed jobs, unbound PVCs, and
  cert-manager certificates about to expire.
- **Metrics** — node CPU/memory heatmaps, top pods, per-service live levels,
  and inline usage bars on the pod list (needs metrics-server).
- **Topology** — an Ingress → Service → Workload → Pod graph with replica
  health and `NoEndpoints` detection.
- **Quotas** — per-namespace ResourceQuota usage against hard limits.
- **19 resource kinds** with a detail drawer: overview, YAML edit with a
  `kubectl diff`-style dry run, log streaming with regex filters, one-shot
  exec, and per-service observability.
- **Operate** — scale, restart, delete, apply, port-forward tracking, and an
  audit trail of every write you make in a session.
- **⌘K omnibox** and keyboard-first navigation.

### The assistant

- Model-agnostic: Anthropic, Ollama, or any OpenAI-compatible server. Add and
  switch backends from the UI without restarting.
- Query the cluster in natural language, diagnose a pod, triage alerts, review
  YAML, explain the topology, summarise logs, and generate manifests.
- Optionally grounded in your own MkDocs site.
- **Voice** on every view: ask aloud, get spoken answers, and issue commands
  behind a confirmation gate.

### Ecosystem, detected not installed

Trivy Operator CVEs, Kyverno/OPA PolicyReports, Argo CD and Flux sync status,
HPA and KEDA autoscaling, and cert-manager expiry — each lights up when its
CRDs exist and degrades quietly when they don't.

### Security posture

KubeAura has no authentication of its own; it acts with your kubeconfig, for
you, on your machine. Everything follows from that:

- Binds `127.0.0.1:7654`. Going wider requires `--allow-remote` and prints a
  warning. 8080 was avoided because it collides with almost everything, and on
  macOS 5000 and 7000 belong to AirPlay.
- Refuses requests whose `Host` is not loopback (DNS rebinding) or whose
  `Origin` does not match the served host (cross-site writes).
- API keys are held in memory only. The config file has no field for them;
  `ai.apiKeyCommand` reads one from your keychain at startup instead.
- Actions you lack RBAC for are disabled in the UI via `SelfSubjectAccessReview`.
- Shared in-cluster deployments ship read-only, without Secrets access, and
  refuse changes to the model backend — otherwise anyone reaching the Service
  could redirect your cluster data to a server they control.

### Install

macOS, Linux, and Windows binaries with published checksums; a
checksum-verifying install script; `go install`; and a multi-arch container
image at `ghcr.io/devganeshg/kubeaura`. The same binary opens as a native
desktop window with `--desktop`.

### Known limitations

- The topology "galaxy" view lazy-loads its 3D library from a CDN, so that one
  view does not work air-gapped. Everything else is embedded in the binary.
- Desktop bundles are not signed or notarized; macOS needs
  `xattr -dr com.apple.quarantine` once.
- Exec is a one-shot command runner, not an interactive TTY.
- Coverage is uneven: configuration, security scanning, and the mutating
  Kubernetes paths are tested; much of the read path and all of the UI is not.
