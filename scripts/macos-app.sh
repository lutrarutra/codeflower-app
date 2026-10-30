#!/bin/sh
# Builds "code flower.app" from the two macOS binaries and packs it into a disk image.
# The app is the launcher itself: started without a terminal, it opens its window
# (codeflower client), with an icon in the menu bar and none in the Dock (LSUIElement).
# It is signed ad hoc only, so macOS asks before the first start.
# Usage: scripts/macos-app.sh ARM64_BINARY AMD64_BINARY VERSION OUT_DIR
set -eu

arm64=$1 amd64=$2 version=${3#v} out=$4
here=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

app="$work/code flower.app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
lipo -create -output "$app/Contents/MacOS/codeflower" "$arm64" "$amd64"

# Info.plist wants a plain dotted version; describe output (v0.1.0-3-gabc) is cut down.
short=$(printf '%s' "$version" | sed -E 's/^([0-9]+(\.[0-9]+){0,2}).*/\1/')
case $short in [0-9]*) ;; *) short=0.0.0 ;; esac

cat > "$app/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>code flower</string>
  <key>CFBundleDisplayName</key><string>code flower</string>
  <key>CFBundleIdentifier</key><string>io.github.lutrarutra.codeflower</string>
  <key>CFBundleExecutable</key><string>codeflower</string>
  <key>CFBundleIconFile</key><string>codeflower</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>$short</string>
  <key>CFBundleVersion</key><string>$version</string>
  <key>LSMinimumSystemVersion</key><string>12.0</string>
  <key>NSHighResolutionCapable</key><true/>
  <key>LSUIElement</key><true/>
</dict>
</plist>
PLIST

iconset="$work/codeflower.iconset"
mkdir -p "$iconset"
for size in 16 32 64 128 256 512; do
  sips -z $size $size "$here/assets/icon.png" --out "$iconset/icon_${size}x${size}.png" >/dev/null
done
for size in 16 32 128 256; do
  cp "$iconset/icon_$((size * 2))x$((size * 2)).png" "$iconset/icon_${size}x${size}@2x.png"
done
rm "$iconset/icon_64x64.png"
iconutil -c icns -o "$app/Contents/Resources/codeflower.icns" "$iconset"

codesign --force --deep --sign - "$app"

mkdir -p "$out" "$work/dmg"
mv "$app" "$work/dmg/"
ln -s /Applications "$work/dmg/Applications"
hdiutil create -quiet -volname "code flower" -srcfolder "$work/dmg" -format UDZO -ov "$out/codeflower-macos.dmg"
echo "$out/codeflower-macos.dmg"
