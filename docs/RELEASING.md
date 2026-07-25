# Releasing KubeMind

Everything is driven by a git tag. Pushing `vX.Y.Z` runs
[`.github/workflows/release.yml`](../.github/workflows/release.yml), which:

1. builds binaries for linux/darwin/windows × amd64/arm64 with GoReleaser,
2. attaches them to a GitHub Release with `checksums.txt`, `LICENSE`, `NOTICE`,
   and `THIRD_PARTY_LICENSES.md`,
3. builds and pushes `ghcr.io/<owner>/kubemind` for amd64 + arm64.

## Cutting a release

```bash
# 1. Everything CI runs, locally first.
make check

# 2. Move the CHANGELOG's [Unreleased] section under the new version heading
#    and add today's date. Update the version badge in README.md.

# 3. Commit, tag, push.
git commit -s -am "release: v0.10.0"
git tag -a v0.10.0 -m "v0.10.0"
git push origin main --follow-tags
```

Watch the Actions tab. When it's green, verify the release actually works
before announcing it:

```bash
curl -sSfL https://raw.githubusercontent.com/devganeshg/kubemind/main/scripts/install.sh | sh
kubemind --version
```

## Versioning

[SemVer](https://semver.org). Pre-1.0, minor versions may break things — say so
in the CHANGELOG when they do. The version is stamped into the binary at build
time via `-ldflags -X main.version=`, and surfaces in `kubemind --version`, the
UI header, and `/api/health`.

## Package managers (optional, off by default)

`brews:` and `scoops:` in [`.goreleaser.yaml`](../.goreleaser.yaml) are
commented out on purpose. GoReleaser resolves `{{ .Env.* }}` at release time, so
enabling a block without its secret **fails the entire release** — including the
binaries that would otherwise have published fine.

To enable Homebrew (`brew install devganeshg/tap/kubemind`):

1. Create a **public** repo `devganeshg/homebrew-tap` (the `homebrew-` prefix is
   required; `brew` maps `devganeshg/tap` to it).
2. Create a fine-grained PAT with **Contents: read and write** on that repo.
3. Add it to this repo as the secret `HOMEBREW_TAP_TOKEN`.
4. Uncomment the `brews:` block and tag a release.

Scoop is the same shape: a public `devganeshg/scoop-bucket` repo and a
`SCOOP_BUCKET_TOKEN` secret, then uncomment `scoops:`.

Until then the install script, `go install`, the release archives, and the
container image all work — that is a complete install story on its own.

## Dependencies

`THIRD_PARTY_LICENSES.md` must match what the binary actually links, because
Apache-2.0 section 4 requires shipping attribution with every distribution. CI
regenerates and diffs it, so after any dependency change:

```bash
make licenses    # then commit the result
```

## Desktop apps

The `.app` / `.exe` / tarball bundles are built by
[`desktop.yml`](../.github/workflows/desktop.yml), which also runs on tags.
They are **not signed or notarized**, so macOS users must clear the quarantine
flag once — this is documented in the README, and it is the first thing people
will report as a bug.
