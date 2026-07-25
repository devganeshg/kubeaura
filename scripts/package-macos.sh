#!/bin/sh
# Package the desktop build into a double-clickable macOS app bundle:
#   dist/KubeMind.app
# Run via `make app` (which builds bin/kubemind with -tags desktop first).
set -eu

cd "$(dirname "$0")/.."

BIN=bin/kubemind
APP=dist/KubeMind.app
ICON_SRC=assets/png/logo-mark-512.png

[ -x "$BIN" ] || { echo "error: $BIN missing — run 'make desktop' first" >&2; exit 1; }

rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"

# Apache-2.0 section 4: attribution travels with every binary distribution.
cp LICENSE NOTICE THIRD_PARTY_LICENSES.md "$APP/Contents/Resources/"

cp "$BIN" "$APP/Contents/MacOS/kubemind"

# Build the .icns from the 512px logo mark using the stock macOS tools.
ICONSET=dist/kubemind.iconset
rm -rf "$ICONSET"
mkdir -p "$ICONSET"
for s in 16 32 64 128 256 512; do
  sips -z $s $s "$ICON_SRC" --out "$ICONSET/icon_${s}x${s}.png" >/dev/null
  d=$((s * 2))
  if [ $d -le 512 ]; then
    sips -z $d $d "$ICON_SRC" --out "$ICONSET/icon_${s}x${s}@2x.png" >/dev/null
  fi
done
iconutil -c icns "$ICONSET" -o "$APP/Contents/Resources/kubemind.icns"
rm -rf "$ICONSET"

# KUBEMIND_DESKTOP=1 makes the binary open its native window instead of a
# browser — .app bundles can't pass CLI flags, so it rides in LSEnvironment.
# The window is Chrome/Edge app mode (chromeless, but Chrome's icon in the
# Dock) because only Chrome's engine has the Web Speech API — the mic is a
# core feature, and the native WKWebView window cannot do voice input at all.
cat > "$APP/Contents/Info.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>KubeMind</string>
  <key>CFBundleDisplayName</key><string>KubeMind</string>
  <key>CFBundleIdentifier</key><string>ai.kubemind.desktop</string>
  <key>CFBundleExecutable</key><string>kubemind</string>
  <key>CFBundleIconFile</key><string>kubemind</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>0.9.0</string>
  <key>NSHighResolutionCapable</key><true/>
  <key>LSMinimumSystemVersion</key><string>11.0</string>
  <key>NSMicrophoneUsageDescription</key>
  <string>KubeMind uses the microphone for voice commands.</string>
  <key>NSSpeechRecognitionUsageDescription</key>
  <string>KubeMind transcribes voice commands to control your cluster.</string>
  <key>LSEnvironment</key>
  <dict>
    <key>KUBEMIND_DESKTOP</key><string>1</string>
  </dict>
</dict>
</plist>
PLIST

echo "packaged $APP — double-click it in Finder, or: open $APP"
