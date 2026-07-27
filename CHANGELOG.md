# Changelog

All notable changes to KubeAura. The format follows
[Keep a Changelog](https://keepachangelog.com); versioning follows
[SemVer](https://semver.org). Pre-1.0, minor versions may break things — when
they do, it will say so here.

## [Unreleased]

## [v0.2.0] — 2026-07-27

Four features and one thing that should never have shipped the way it did.

If you use the AI Assistant with a hosted model, read the first section —
what leaves your machine has changed, and so has your ability to see it.

### Added

- **Evidence envelope for model calls.** The Assistant is the one part of
  KubeAura that can send cluster state to a third party. Troubleshooting used
  to marshal the entire pod object — inline environment values, the
  `last-applied-configuration` annotation (a verbatim copy of your manifest),
  and uncapped events. Evidence is now redacted by rule: every inline
  `env[].value` is dropped while names and `valueFrom`/`envFrom` references
  survive, credential-shaped annotations go, `command`/`args` are scrubbed,
  logs cap at 32 KiB and events at 40/16 KiB. The payload is an explicit
  allow-list, so a new field in a future client-go cannot start being sent
  silently. Every call produces an envelope — resource and UID, the rules that
  fired with counts and bytes, the log window, the destination, and a SHA-256
  of the payload — shown for approval before an off-machine call and recorded
  in the audit trail with the answer. Secret contents are still read nowhere.
- **Helm.** Releases, history, values, manifest and notes, decoded from Helm's
  own storage secrets — no new dependency, works without the helm binary.
  Upgrade, rollback and uninstall shell out to real `helm` and are
  loopback-only.
- **Fleet view.** Every kubeconfig context queried concurrently, so an
  unreachable cluster becomes a row with its error instead of a hung page.
- **Compliance report export.** Vulnerabilities, policy results and RBAC
  posture as JSON, CSV, Markdown or HTML.
- **Collapsible navigation** (⌘B) and a **stop control for the voice
  assistant** — the mic is now a toggle, and Escape stops speech.

### Changed

- `/api/ai/troubleshoot` and `/api/ai/logsummary` return `{answer, evidence}`
  instead of `{answer}`, and accept `{"preview": true}` to return the envelope
  without calling the model. Readers of `answer` are unaffected.
- RBAC now resolves per request from the active context. It previously used a
  validator captured at startup, which reported the previous cluster's answers
  after a context switch.

### Fixed

- **The compliance check never checked anything.** `/api/security/compliance`
  returned `{"compliant": true, "violations": []}` for any input, behind a
  TODO. It now resolves the image against Trivy Operator's reports and rejects
  unknown policy names. A check that cannot run is reported as `checked:
  false` with the reason and never counts as a pass.
- `kubeaura --version` printed `0.1.1` from a release download but `v0.1.1`
  from `go install`. Both report the tag form now.

### Upgrading

No configuration changes. If you script against the compliance endpoint,
note that it can now return 400 for a policy name it previously accepted —
that acceptance was never meaningful.

## [v0.1.1] — 2026-07-26

Repairs the v0.1.0 release pipeline. No behaviour changes to the cockpit
itself; if v0.1.0 works for you, the only thing you gain is a binary that can
tell you which version it is.

### Fixed

- `kubeaura --version` reported `dev` when installed with `go install`, which
  is what the README recommends. It now falls back to the module version the
  Go toolchain embeds.
- The container image never published for v0.1.0: the release workflow pulled
  its cross-build helper from Docker Hub with no retry, and that pull timed
  out. `ghcr.io/devganeshg/kubeaura:0.1.0` now exists — the in-cluster
  manifests and Helm chart pointed at an image that was never pushed.
- The Linux desktop build failed on `webkit2gtk-4.0`, which Ubuntu 24.04 no
  longer ships. It builds on 22.04 now, which also links an older glibc.
- The release verification workflow never ran, because releases created by
  `GITHUB_TOKEN` do not fire the `release: published` event. v0.1.0 shipped
  without its install paths ever being exercised.

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
