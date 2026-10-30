// Package assets holds the app's icons.
package assets

import _ "embed"

// Icon is the app icon, 128×128 PNG.
//
//go:embed icon.png
var Icon []byte

// TrayTemplate is the tulip's outline for the macOS menu bar, black on
// transparent, which macOS recolours for light and dark menu bars.
//
//go:embed tray-template.png
var TrayTemplate []byte
