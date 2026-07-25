#!/bin/sh
# Regenerate THIRD_PARTY_LICENSES.md from the modules actually linked into the
# binary (not the whole module graph), so the list matches what we ship.
#
#   sh scripts/gen-licenses.sh
#
# Run this after any dependency change; CI verifies the file is up to date.
set -e
OUT=THIRD_PARTY_LICENSES.md
TMPBIN=$(mktemp -d)/kubeaura
MC=$(go env GOMODCACHE)

CGO_ENABLED=0 go build -o "$TMPBIN" ./cmd/kubeaura

cat > "$OUT" <<'HDR'
# Third-party licenses

KubeAura is distributed as a single statically linked binary. The binary
embeds the Go modules listed below. Each dependency remains under its own
license and copyright; this file is provided to satisfy the attribution
requirements of those licenses (notably Apache-2.0 section 4).

Full license texts live in each module's source repository and in your local
Go module cache under `$(go env GOMODCACHE)/<module>@<version>/LICENSE`.

**Weak-copyleft notice (MPL-2.0):** `github.com/hashicorp/go-cleanhttp` and
`github.com/hashicorp/go-retryablehttp` are licensed under the Mozilla Public
License 2.0. Their source is available unmodified at the URLs below; KubeAura
makes no modifications to either module.

| Module | Version | License |
| ------ | ------- | ------- |
HDR

go version -m "$TMPBIN" | awk '$1=="dep"{print $2"@"$3}' | while IFS= read -r m; do
  mod=${m%@*}; ver=${m##*@}
  esc=$(printf '%s' "$m" | sed 's/\([A-Z]\)/!\l\1/g')
  lf=$(ls "$MC/$esc" 2>/dev/null | grep -iE "^(licen[cs]e|copying)" | head -1)
  L=UNKNOWN
  if [ -n "$lf" ]; then
    txt=$(head -25 "$MC/$esc/$lf" | tr -s ' \n' ' ')
    case "$txt" in
      *"Apache License"*) L="Apache-2.0";;
      *"ISC License"*) L="ISC";;
      *"Mozilla Public License"*) L="MPL-2.0";;
      *"MIT License"*|*"Permission is hereby granted, free of charge"*) L="MIT";;
      *"Neither the name"*) L="BSD-3-Clause";;
      *"Redistribution and use in source and binary forms"*) L="BSD-2-Clause";;
    esac
  fi
  printf '| [%s](https://%s) | %s | %s |\n' "$mod" "$mod" "$ver" "$L" >> "$OUT"
done

cat >> "$OUT" <<'FTR'

## Runtime assets loaded by the web UI

The topology "galaxy" view lazy-loads
[3d-force-graph](https://github.com/vasturiano/3d-force-graph) (MIT, Copyright
Vasco Asturiano) from the unpkg CDN on first use. It is **not** bundled in the
binary, and no other view makes an outbound request. Every other asset in the
UI is embedded via `go:embed`.

Regenerate this file with `sh scripts/gen-licenses.sh`.
FTR
echo "wrote $OUT"
