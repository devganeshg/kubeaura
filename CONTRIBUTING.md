# Contributing to KubeAura

Thanks for your interest! KubeAura is an open-source, operator-class
Kubernetes UI (in the spirit of k9s / Lens / Headlamp) with a built-in AI Assistant.

## Ground rules

- **It's an operator tool, not a SaaS backend.** Keep changes aligned with the
  single-binary, per-operator, stateless model (see
  `docs/internal/architecture.txt` → ARCHITECTURE MODEL). No shared caches,
  message buses, or HA tiers.
- **Loopback and no-auth are load-bearing.** KubeAura authenticates nobody, so
  the defaults in `internal/api/guard.go` and `config.DefaultAddr` are what keep
  it safe. Do not widen them without saying why in the PR.
- **Never persist secrets.** The tool acts only with the operator's own
  kubeconfig credentials and honors their RBAC.
- **Scale via the API server**, not our own storage — prefer informers,
  server-side pagination, and field/label selectors over full LIST calls.

## Development

```bash
# Prereqs: the Go version in go.mod, and a reachable cluster (kind works great)
make build && ./bin/kubeaura    # http://127.0.0.1:8080

# Enable the AI Assistant — a hosted key, or nothing at all with Ollama
export ANTHROPIC_API_KEY=sk-ant-...
KUBEAURA_AI_PROVIDER=ollama ./bin/kubeaura
```

The web UI is embedded with `go:embed`, so edits to `web/static/index.html`
require a rebuild to show up.

### Layout

| Package | Responsibility |
|---------|----------------|
| `cmd/kubeaura`  | entrypoint |
| `internal/config`| flags + env + config-file resolution |
| `internal/k8s`   | client-go: list/summary/logs/scale/restart/delete/apply |
| `internal/ai`    | AI assistant + pluggable model providers |
| `internal/api`   | HTTP JSON API + routing |
| `web/`           | embedded single-page UI (`go:embed`) |

## Before you open a PR

```bash
make vet && make test
gofmt -l .                  # should print nothing
sh scripts/gen-licenses.sh  # if you changed dependencies; commit the result
```

- Keep PRs focused; describe the user-facing change.
- New Kubernetes read paths should use pagination/selectors where relevant.
- UI changes should degrade gracefully when the AI Assistant is disabled.

## Reporting bugs / ideas

Open an issue with: cluster type (kind/EKS/GKE/…), `kubectl version`, steps to
reproduce, and what you expected. For features, describe the operator workflow
you're trying to improve.

## Sign your work (DCO)

Contributions are accepted under the
[Developer Certificate of Origin](https://developercertificate.org): a short
statement that you wrote the patch, or otherwise have the right to submit it
under Apache-2.0. Certify it by signing off each commit:

```bash
git commit -s -m "fix: keep the port-forward table sorted"
```

That appends a `Signed-off-by: Your Name <you@example.com>` line. There is no
CLA to sign, and contributions remain licensed under Apache-2.0.
