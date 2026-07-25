# Security Policy

## Supported Versions

Only the latest release of KubeAura receives security fixes.

## Reporting a Vulnerability

Please **do not** open a public GitHub issue for security vulnerabilities.
Instead, report them privately via
[GitHub Security Advisories](https://github.com/devganeshg/kubeaura/security/advisories/new).

You can expect an initial response within a week. Once a fix is available, the
vulnerability will be disclosed in the release notes.

## The threat model

KubeAura is an **operator tool** (like k9s or Lens), not a multi-tenant
service. It has **no authentication or authorization of its own**: it acts with
the permissions of the kubeconfig it is given, for the single person running
it. Everything below follows from that.

### What KubeAura defends against

- **DNS rebinding.** Requests whose `Host` header is not loopback are refused
  unless you explicitly set `KUBEAURA_ALLOW_REMOTE=1` / `--allow-remote`.
  Without this, any website you visit could resolve its own name to
  `127.0.0.1` and drive your local API.
- **Cross-site requests.** Any request carrying an `Origin` that does not match
  the host being served is refused, so another site cannot POST to
  `/api/delete` or `/api/exec` in the background.
- **Accidental exposure.** The listen address defaults to `127.0.0.1:7654`, and
  binding anything wider prints a warning at startup.
- **Secret leakage to disk.** AI provider keys live in memory only. The config
  file has no field for them; `ai.apiKeyCommand` fetches one from your keychain
  at startup instead of storing it. Keys are redacted in every API response.

### What it does not, and cannot, defend against

- **Anyone with access to your machine or your kubeconfig.** They can do what
  you can do, with or without KubeAura.
- **A shared instance without an authenticating proxy.** In-cluster deployments
  must relax the loopback check to work at all, so **everyone who can reach the
  Service acts as the ServiceAccount**, and the audit trail cannot distinguish
  them. The shipped manifests are therefore read-only, have no Secrets access,
  and expect you to authenticate the Ingress (oauth2-proxy, your SSO, your
  service mesh). Port-forwarding avoids the problem entirely.
- **Repointing the model backend on a shared instance.** Whoever chooses the
  AI connection chooses where cluster snapshots and pod logs are sent. On a
  loopback run that is you, and it is not a boundary. On a shared instance it
  would be — so when `KUBEAURA_ALLOW_REMOTE` is set, the endpoints that add,
  activate, remove, or discover model connections refuse writes. Configure the
  backend through the Deployment's environment instead. (The same gate closes
  the SSRF primitive in model discovery, which would otherwise let a viewer make
  the pod fetch arbitrary URLs from inside your cluster network.)
- **What you send to a hosted model.** When the assistant is pointed at a hosted
  API, cluster snapshots — and, when troubleshooting, a pod's spec, events, and
  logs — are sent to that provider. Use Ollama or a local OpenAI-compatible
  server if that is unacceptable.
- **The topology galaxy's CDN fetch.** That one view loads a 3D library from
  unpkg at runtime, which reveals your IP to that CDN and does not work
  air-gapped. Nothing else in the UI makes an outbound request.

## Hardening checklist for operators

- Run it with a kubeconfig scoped to what you actually want it to manage — the
  tool honors RBAC but does not add any of its own.
- Keep the default loopback bind. If you must widen it, put authentication in
  front and treat the port as fully privileged.
- For shared deployments, keep `rbac.readOnly=true` and
  `rbac.allowSecrets=false` until your ingress requires a login.
- Verify release downloads against the published `checksums.txt`.
