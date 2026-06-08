//go:build darwin

package main

import (
	"bytes"
	"image"
	"image/png"
	"testing"
)

// TestStatusIconRendersTintedGlyph verifies statusIcon emits a valid iconSize
// PNG for every VM state, that the embedded "b" glyph is actually present
// (some opaque pixels), and that every opaque pixel carries the state's tint
// color (so the mask is composited, not just a blank/solid fill).
func TestStatusIconRendersTintedGlyph(t *testing.T) {
	cases := []struct {
		state   vmState
		name    string
		r, g, b uint8
	}{
		{vmStopped, "stopped", grayR, grayG, grayB},
		{vmHealthy, "healthy", greenR, greenG, greenB},
		{vmWedged, "wedged", amberR, amberG, amberB},
		{vmUnknown, "unknown", amberR, amberG, amberB},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			img, err := png.Decode(bytes.NewReader(statusIcon(tc.state)))
			if err != nil {
				t.Fatalf("decode statusIcon(%s): %v", tc.name, err)
			}
			if got := img.Bounds().Size(); got.X != iconSize || got.Y != iconSize {
				t.Fatalf("size = %v, want %dx%d", got, iconSize, iconSize)
			}
			nrgba, ok := img.(*image.NRGBA)
			if !ok {
				t.Fatalf("decoded image is %T, want *image.NRGBA", img)
			}
			var opaque int
			for y := 0; y < iconSize; y++ {
				for x := 0; x < iconSize; x++ {
					p := nrgba.NRGBAAt(x, y)
					if p.A == 0 {
						continue
					}
					opaque++
					if p.R != tc.r || p.G != tc.g || p.B != tc.b {
						t.Fatalf("pixel (%d,%d) = %v, want tint (%d,%d,%d)", x, y, p, tc.r, tc.g, tc.b)
					}
				}
			}
			if opaque == 0 {
				t.Fatal("no opaque pixels: glyph mask did not render")
			}
		})
	}
}

// TestBrandAssetsEmbedded guards the go:embed wiring: the menu-bar glyph decoded
// at startup and the app icon bytes are present (catches a missing/empty asset
// before it ships in a bundle).
func TestBrandAssetsEmbedded(t *testing.T) {
	if menubarGlyph == nil || menubarGlyph.Bounds().Empty() {
		t.Fatal("menubarGlyph is nil/empty")
	}
	if len(appIconICNS) == 0 {
		t.Fatal("appIconICNS is empty")
	}
	// .icns magic: "icns" at offset 0.
	if !bytes.HasPrefix(appIconICNS, []byte("icns")) {
		t.Fatalf("appIconICNS missing 'icns' magic (len=%d)", len(appIconICNS))
	}
}
