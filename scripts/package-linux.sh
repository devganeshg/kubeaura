#!/bin/sh
# Cross-compile and package the Linux desktop app (Headlamp-style tarball):
#   dist/kubeaura-linux-<arch>.tar.gz
# Run via `make app-linux` (ARCH=arm64 for ARM machines).
#
# Built without the `desktop` tag: the native webview needs cgo, which does
# not cross-compile from macOS. The desktop window instead uses Chrome or
# Chromium app mode (see desktop_chrome.go), resolved via $PATH.
set -eu

cd "$(dirname "$0")/.."

VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
ARCH=${ARCH:-amd64}
NAME=kubeaura-linux-$ARCH
STAGE=dist/$NAME
TARBALL=dist/$NAME.tar.gz
ICON_SRC=assets/png/logo-mark-512.png

rm -rf "$STAGE" "$TARBALL"
mkdir -p "$STAGE"

# Apache-2.0 section 4: attribution travels with every binary distribution.
cp LICENSE NOTICE THIRD_PARTY_LICENSES.md "$STAGE/"

CGO_ENABLED=0 GOOS=linux GOARCH=$ARCH go build -trimpath \
  -ldflags "-s -w -X main.version=$VERSION" \
  -o "$STAGE/kubeaura" ./cmd/kubeaura

cp "$ICON_SRC" "$STAGE/kubeaura.png"

# Exec is rewritten to an absolute path by install.sh; this default works
# when the binary is already on $PATH.
cat > "$STAGE/kubeaura.desktop" <<'DESKTOP'
[Desktop Entry]
Type=Application
Name=KubeAura
Comment=AI-assisted Kubernetes dashboard for your existing kubeconfig
Exec=kubeaura --desktop
Icon=kubeaura
Terminal=false
Categories=Development;System;
Keywords=kubernetes;k8s;cluster;ai;
DESKTOP

cat > "$STAGE/install.sh" <<'INSTALL'
#!/bin/sh
# Per-user install: binary, launcher icon, and app-menu entry under ~/.local.
set -eu
cd "$(dirname "$0")"

BIN_DIR=${BIN_DIR:-$HOME/.local/bin}
APP_DIR=$HOME/.local/share/applications
ICON_DIR=$HOME/.local/share/icons/hicolor/512x512/apps

mkdir -p "$BIN_DIR" "$APP_DIR" "$ICON_DIR"
install -m 755 kubeaura "$BIN_DIR/kubeaura"
install -m 644 kubeaura.png "$ICON_DIR/kubeaura.png"
sed "s|^Exec=.*|Exec=$BIN_DIR/kubeaura --desktop|" kubeaura.desktop \
  > "$APP_DIR/kubeaura.desktop"
update-desktop-database "$APP_DIR" 2>/dev/null || true

echo "installed — launch 'KubeAura' from your app menu, or run: $BIN_DIR/kubeaura"
echo "the desktop window needs Chrome or Chromium on \$PATH; without one, run 'kubeaura' for browser mode"
INSTALL
chmod +x "$STAGE/install.sh"

cat > "$STAGE/README.txt" <<EOF
KubeAura $VERSION — Linux ($ARCH)

Run ./install.sh to install for your user (binary in ~/.local/bin plus an
app-menu entry), or just run ./kubeaura directly.

The desktop window uses Chrome/Chromium app mode; with no Chromium-family
browser installed, KubeAura falls back to serving http://localhost:8080
in your default browser. It reads your existing kubeconfig (~/.kube/config).
EOF

tar -czf "$TARBALL" -C dist "$NAME"

echo "packaged $TARBALL — copy to a Linux machine, extract, run ./install.sh"
