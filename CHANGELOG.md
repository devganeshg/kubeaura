# Changelog

All notable changes to KubeMind. The format follows
[Keep a Changelog](https://keepachangelog.com); versioning follows
[SemVer](https://semver.org) (pre-1.0: minor versions may break).

## [Unreleased] — renamed to KubeMind, open-source readiness

**The project is now called KubeMind.** It was KubePilot, a name already in use
by an open-source Go project in the same category (kubepilot.org), a commercial
managed-Kubernetes company (kubepilot.tech), a GitOps product, a VS Code
extension, and ten public GitHub repositories. Renaming before the first public
release costs nothing; renaming after it would break every install command and
link.

What changed for you:

| Was | Now |
| --- | --- |
| `kubepilot` | `kubemind` |
| `github.com/devganeshg/kubepilot-ai` | `github.com/devganeshg/kubemind` |
| `KUBEPILOT_*` | `KUBEMIND_*` (old names still work, with a deprecation notice) |
| `~/.config/kubepilot/config.yaml` | `~/.config/kubemind/config.yaml` |
| `KubePilot.app` | `KubeMind.app` |

No API endpoint, flag, or config key changed.

The logo follows: the wordmark now reads Kube**Mind**, and the paper-plane
glyph — a "pilot" metaphor — is replaced by a synapse (a core node with three
linked satellites), which reads as both a neural network and a cluster
topology. The hexagon and the #ff4000 → #ff0072 gradient are unchanged.

The rest of this entry is preparation for the first public release:
attribution, safe defaults, and an installation path that does not require a Go
toolchain.

### Added

- **Install script and package managers** — `scripts/install.sh` (checksum
  verified) plus GoReleaser-published Homebrew tap and Scoop bucket.
- **Command-line flags** — `--help`, `--version`, `--addr`, `--kubeconfig`,
  `--context`, `--ai-provider`, `--ai-model`, `--no-browser`, `--allow-remote`.
- **Config file** — `kubemind config init` writes an annotated
  `~/.config/kubemind/config.yaml`. Precedence is flags > environment > file.
  API keys are never stored in it; `ai.apiKeyCommand` fetches one from your
  keychain instead, which is how the desktop app can use a key at all.
- **`NOTICE` and `THIRD_PARTY_LICENSES.md`**, generated from the shipped binary
  by `make licenses` and included in every release archive and app bundle, as
  Apache-2.0 section 4 requires. CI fails if the list goes stale.
- Contributor scaffolding: `CODE_OF_CONDUCT.md`, issue and PR templates, DCO
  sign-off, `.env.example`.

### Changed

- **The listen address now defaults to loopback** instead of every
  interface. Binding wider prints a warning and requires `--allow-remote`.
- **In-cluster defaults are read-only** (`rbac.readOnly=true`) and no longer
  grant Secrets access (`rbac.allowSecrets=false`); a shared instance
  authenticates nobody until you put auth in front of the Ingress.
- The AI assistant is no longer called "Copilot", and the internal "Jarvis"
  naming is gone from the UI — both were other companies' trademarks. No
  endpoint, flag, or environment variable changed.
- Default Anthropic model is now `claude-opus-5`.
- README documents installation the way k9s does, and no longer claims "no
  external assets" (the topology galaxy loads its 3D library from a CDN).

- **Default port is now 7654**, not 8080. A developer machine running a
  Kubernetes tool very likely already has something on 8080, and on macOS
  ports 5000 and 7000 belong to AirPlay. If the port is busy anyway, KubeMind
  now steps to the next free one in *every* mode instead of only in the
  desktop app — unless you asked for a specific `--addr`, in which case a
  collision still fails loudly.
- **A boot sequence replaces the blank first paint** — it reports real
  progress (kubeconfig → cluster → permissions → ecosystem) and leaves as soon
  as the data lands, rather than holding you on a timed splash.

### Security

- **DNS-rebinding protection** — requests whose `Host` is not loopback are
  refused unless remote serving is explicitly enabled.
- **Cross-site request protection** — any request whose `Origin` does not match
  the served host is refused, so another site cannot drive `/api/delete` or
  `/api/exec` in the background.

## [v0.9.0] — 2026-07-06

First versioned release: the core cockpit plus the CNCF detect-don't-install
integration wave.

### Added

- **Quota dashboard** — per-namespace ResourceQuota usage vs hard limits with
  progress bars, peak-utilization cards, and near-limit highlighting.
- **Security view** — image CVE reports read from the Trivy Operator
  (severity cards, severity-mix donut, most-exposed bars, per-workload
  findings with fix versions). `/api/security/vulnerabilities`.
- **Policy view** — Kyverno / OPA compliance via the standard
  `wgpolicyk8s.io` PolicyReport CRD (pass/fail/warn/error cards, compliance
  donut, violations-by-namespace bars, expandable findings). `/api/policy`.
- **GitOps view** — Argo CD Applications + Flux Kustomizations/HelmReleases
  sync & health, problems-first, with failure messages. `/api/gitops`.
- **Autoscaling view** — core HPA status (replica flow, live metric
  utilization, AtMax/Scaling/Inactive states, capacity-headroom bars) plus
  KEDA ScaledObjects with trigger types. `/api/autoscaling`.
- **cert-manager expiry alerts** — certificates expiring <14d (warning),
  <7d or expired (critical) in the Alerts view.
- **Pods table Req/Lim column** — summed container requests/limits per pod,
  red-highlighted when live usage is ≥90% of a limit.
- **Topology upgrades** — workload replica health, NoEndpoints service
  detection, Job→CronJob collapsing, hover edge-highlighting, tooltips,
  dot-grid canvas, per-namespace issue summary.
- **Dark/light themes** (persisted, follows OS preference) with a
  glassmorphism UI, ambient background glows, and an SVG icon set.
- **Parent-child navigation** with persisted collapse state, ordered
  Observe → Workloads → Network → Platform → Config → Cluster → Access →
  Operate.
- **Logo & branding** — gradient hexagon + paper-plane mark, favicon, and a
  version badge in the header (`/api/health` reports the build version).

### Fixed

- RBAC validator no longer panics when constructed without a cluster client.
- Artifactory handlers validate request parameters before checking
  integration configuration (400 before 503).
- API test suite compiles and runs (was silently broken by an unused import).
