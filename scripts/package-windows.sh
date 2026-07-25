#!/bin/sh
# Cross-compile and package the Windows desktop app:
#   dist/KubeMind-windows-<arch>.zip
# Run via `make app-windows` (ARCH=arm64 for Windows-on-ARM).
#
# Built without the `desktop` tag: the native webview needs cgo, which does
# not cross-compile from macOS. The desktop window instead uses Chrome/Edge
# app mode (see desktop_chrome.go) — Edge ships with Windows 10/11, so the
# window is always available.
set -eu

cd "$(dirname "$0")/.."

VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
ARCH=${ARCH:-amd64}
STAGE=dist/windows-$ARCH
ZIP=dist/KubeMind-windows-$ARCH.zip

rm -rf "$STAGE" "$ZIP"
mkdir -p "$STAGE"

# Apache-2.0 section 4: attribution travels with every binary distribution.
cp LICENSE NOTICE THIRD_PARTY_LICENSES.md "$STAGE/"

# -H=windowsgui builds a GUI-subsystem exe: double-clicking it opens no
# console window. The trade-off is that terminal output is invisible, which
# is fine for an app always launched by icon.
CGO_ENABLED=0 GOOS=windows GOARCH=$ARCH go build -trimpath \
  -ldflags "-s -w -H=windowsgui -X main.version=$VERSION" \
  -o "$STAGE/KubeMind.exe" ./cmd/kubemind

# Desktop mode rides on KUBEMIND_DESKTOP=1 (a zip can't ship a shortcut
# with CLI arguments); `start` detaches so the .bat's console flash is brief.
printf '%s\r\n' \
  '@echo off' \
  'set KUBEMIND_DESKTOP=1' \
  'start "" "%~dp0KubeMind.exe"' \
  > "$STAGE/KubeMind.bat"

printf '%s\r\n' \
  "KubeMind $VERSION — Windows" \
  '' \
  'Double-click KubeMind.bat to launch the desktop app.' \
  '' \
  'The app window uses Chrome or Edge app mode (Edge ships with Windows).' \
  'It reads your existing kubeconfig (%USERPROFILE%\.kube\config).' \
  '' \
  'To pin it: right-click KubeMind.exe > Create shortcut, then open the' \
  'shortcut Properties and append " --desktop" to the Target field.' \
  > "$STAGE/README.txt"

(cd "$STAGE" && zip -qr "../../$ZIP" .)

echo "packaged $ZIP — copy to a Windows machine, unzip, run KubeMind.bat"
