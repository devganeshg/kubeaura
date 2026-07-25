#!/bin/sh
# KubeAura installer for macOS and Linux.
#
#   curl -sSfL https://raw.githubusercontent.com/devganeshg/kubeaura/main/scripts/install.sh | sh
#
# Downloads the release archive for your platform, verifies it against the
# published checksums, and installs the binary. Nothing else on your system is
# touched: no daemon, no config written, no shell profile edited.
#
# Environment:
#   KUBEAURA_VERSION   version to install (default: latest release)
#   KUBEAURA_BIN_DIR   install directory (default: /usr/local/bin, else ~/.local/bin)
set -eu

REPO="devganeshg/kubeaura"
VERSION="${KUBEAURA_VERSION:-}"

say()  { printf '%s\n' "$*"; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required but not installed"; }

need uname
need tar
if command -v curl >/dev/null 2>&1; then
  fetch() { curl -sSfL "$1" -o "$2"; }
  fetch_stdout() { curl -sSfL "$1"; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -qO "$2" "$1"; }
  fetch_stdout() { wget -qO- "$1"; }
else
  die "curl or wget is required"
fi

# --- platform -----------------------------------------------------------
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  darwin|linux) ;;
  *) die "unsupported OS '$os'. Windows users: use Scoop, or download the .zip from
       https://github.com/$REPO/releases" ;;
esac

arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) die "unsupported architecture '$arch'. Build from source: go install github.com/$REPO/cmd/kubeaura@latest" ;;
esac

# --- version ------------------------------------------------------------
if [ -z "$VERSION" ]; then
  VERSION=$(fetch_stdout "https://api.github.com/repos/$REPO/releases/latest" \
    | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)
  [ -n "$VERSION" ] || die "could not determine the latest release. Set KUBEAURA_VERSION, or check
       https://github.com/$REPO/releases"
fi
num=${VERSION#v}

# --- download and verify ------------------------------------------------
archive="kubeaura_${num}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/$VERSION"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

say "Downloading kubeaura $VERSION ($os/$arch)…"
fetch "$base/$archive" "$tmp/$archive" || die "download failed: $base/$archive"

if fetch "$base/checksums.txt" "$tmp/checksums.txt" 2>/dev/null; then
  if command -v sha256sum >/dev/null 2>&1; then
    sum=$(sha256sum "$tmp/$archive" | cut -d' ' -f1)
  elif command -v shasum >/dev/null 2>&1; then
    sum=$(shasum -a 256 "$tmp/$archive" | cut -d' ' -f1)
  else
    sum=""
    say "warning: no sha256sum/shasum available, skipping checksum verification"
  fi
  if [ -n "$sum" ]; then
    grep -q "$sum" "$tmp/checksums.txt" || die "checksum mismatch for $archive — do not use this download"
    say "Checksum verified."
  fi
else
  say "warning: checksums.txt not published for $VERSION, skipping verification"
fi

tar -xzf "$tmp/$archive" -C "$tmp"
[ -f "$tmp/kubeaura" ] || die "archive did not contain a kubeaura binary"

# --- install ------------------------------------------------------------
if [ -n "${KUBEAURA_BIN_DIR:-}" ]; then
  bindir="$KUBEAURA_BIN_DIR"
elif [ -w /usr/local/bin ] 2>/dev/null; then
  bindir=/usr/local/bin
else
  bindir="$HOME/.local/bin"
fi
mkdir -p "$bindir"
install -m 0755 "$tmp/kubeaura" "$bindir/kubeaura" 2>/dev/null \
  || { cp "$tmp/kubeaura" "$bindir/kubeaura" && chmod 0755 "$bindir/kubeaura"; }

say ""
say "Installed $bindir/kubeaura"
case ":$PATH:" in
  *":$bindir:"*) say "Run:  kubeaura" ;;
  *) say "  $bindir is not on your PATH. Add it:"
     say "    echo 'export PATH=\"$bindir:\$PATH\"' >> ~/.profile"
     say "  Then run:  kubeaura" ;;
esac
say ""
say "KubeAura uses your existing kubeconfig and serves http://127.0.0.1:8080."
say "Optional AI setup:  kubeaura config init"
