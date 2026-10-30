package tray

import (
	"bytes"
	"image/png"
	"testing"

	"codeflower-launcher/assets"
)

func TestTheMenuBarIconIsATemplate(t *testing.T) {
	image, err := png.Decode(bytes.NewReader(assets.TrayTemplate))
	if err != nil {
		t.Fatal(err)
	}
	bounds := image.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			if r, g, b, _ := image.At(x, y).RGBA(); r|g|b != 0 {
				t.Fatalf("pixel %d,%d is not black: only its alpha may vary", x, y)
			}
		}
	}
}
