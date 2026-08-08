# Changelog

All notable changes to KubeAura. The format follows
[Keep a Changelog](https://keepachangelog.com); versioning follows
[SemVer](https://semver.org). Pre-1.0, minor versions may break things — when
they do, it will say so here.

## [Unreleased]

### Added

- **Alerts remember.** Rule evaluation was stateless: every refresh produced a
  fresh list with no way to tell a problem that started two minutes ago from one
  that has been firing for three days. Alerts now carry a stable fingerprint and
  the tracker behind it reports how long each has been firing, how many
  evaluations it survived, and which are new. Within a severity band the newest
  sort first — what just broke is what you want to see. Acknowledge one and it
  sinks out of the way; the acknowledgement is dropped automatically when the
  alert resolves, so a recurrence is surfaced again rather than staying silently
  triaged. Resolved alerts are listed for 30 minutes, because seeing a fix take
  is part of triage too. State is in memory and per-process, like the rest of
  KubeAura.
- **A "what changed?" view.** The first question of most incidents had no answer
  anywhere in the tool. `/api/changes` and the new Changes view build a timeline
  from what the cluster already records: every Helm revision with its deploy
  timestamp, every Deployment rollout (the ReplicaSet a rollout leaves behind
  dates it, so `kubectl set image` shows up as readily as a Helm upgrade), Argo
  CD sync completions, and nodes joining. No new dependency and nothing to
  install — the data was being read already and discarded.
- **Alerts name their suspects.** Each alert is correlated against changes that
  landed in the 15 minutes before it started firing. A change to the alerting
  object itself outranks one that merely shares its namespace, matched through
  the generated pod-name suffix so `api-gateway-7d98b64fc9-x2k1p` is not mistaken
  for `api`. Advisory `info` alerts are left alone — they are standing advice,
  not incidents, and pointing at the preceding deploy would imply a cause that
  is not there. It is labelled as correlation, because that is what it is.
- **Prometheus is queried, not just discovered.** KubeAura already found
  Prometheus and only ever deep-linked to it, which left the tool with no past:
  metrics-server holds about a minute of data, so "is this getting worse?" was
  unanswerable. `/api/prom/query`, `/api/prom/history` and `/api/prom/status`
  reach it through the API server's service proxy — the in-cluster service URL
  is not routable from a laptop, but the proxy is, and it reuses the kubeconfig
  credentials that are already loaded. That means it works wherever kubectl
  works, with no port-forward, no configuration and no extra listening socket.
  Curated queries cover pod/node CPU and memory and restart counts so common
  questions need no PromQL. Clusters without Prometheus report `available:
  false` and the UI hides the charts, exactly as it already does for
  metrics-server.

### Security

- **The evidence envelope now covers every model call, not two of them.**
  v0.2.0 introduced redaction, caps and hash-stamped disclosure, but only wired
  them into `/api/ai/troubleshoot` and `/api/ai/logsummary`. The other six AI
  endpoints built their payload inline and sent it with no cap, no scrubbing,
  no envelope and no audit line.

  The consequence that matters: **AI manifest review sent Secret contents to
  your model provider.** Asking KubeAura to review a `Secret` fetched the live
  object, which is stripped of server noise but nothing else, and posted its
  entire base64 `data` block to Anthropic or OpenAI — with no record that it
  had happened. If you reviewed a Secret with a hosted backend configured,
  treat those values as disclosed to that provider and rotate them.

  Reviewing a Secret now sends its keys and never its values. Reviewing
  anything else applies the same rules the pod path already applied — inline
  `env` values dropped, credential-shaped annotations dropped, ConfigMap values
  scrubbed — matched structurally, so they hold for every kind and for pod
  templates nested at any depth.

### Changed

- **Cluster snapshots are capped and scrubbed.** `/api/ai/query`, its streaming
  twin, `/api/ai/triage` and `?explain=1` on topology now go through the
  evidence layer. These carry flat summary rows rather than objects, so nothing
  here was leaking a pod spec — but event messages are verbatim controller
  output and routinely quote a connection string, and 5 kinds × 200 rows had no
  byte ceiling at all. Snapshots now cap at 48 KiB, trimming the largest list
  first so the small load-bearing ones survive, and free text is scrubbed.
- **Every AI call is audited.** Each one records the evidence hash, the byte
  count and the destination backend — never the evidence itself. `ai.generate`
  gets a line too, though it carries no cluster state.
- **Preview works everywhere.** `"preview": true` on review and query, and
  `?preview=1` on triage, return the envelope without contacting a model.
- The streaming query endpoint emits an `{"type":"evidence"}` event before the
  first token, so the disclosure arrives while the answer is still being
  written.

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
