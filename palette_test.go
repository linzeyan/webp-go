package gowebp

import (
    "bytes"
    "image"
    "image/color"
    "image/draw"
    "testing"
)

// decodeNRGBA encodes img, decodes it back, and returns the result as an
// *image.NRGBA for exact pixel comparison.
func decodeNRGBA(t *testing.T, data []byte) *image.NRGBA {
    t.Helper()
    decoded, err := Decode(bytes.NewReader(data))
    if err != nil {
        t.Fatalf("decode: %v", err)
    }
    out := image.NewNRGBA(decoded.Bounds())
    draw.Draw(out, out.Bounds(), decoded, decoded.Bounds().Min, draw.Src)
    return out
}

func assertSamePixels(t *testing.T, want, got *image.NRGBA) {
    t.Helper()
    if want.Bounds() != got.Bounds() {
        t.Fatalf("bounds differ: %v vs %v", want.Bounds(), got.Bounds())
    }
    b := want.Bounds()
    for y := b.Min.Y; y < b.Max.Y; y++ {
        for x := b.Min.X; x < b.Max.X; x++ {
            if want.NRGBAAt(x, y) != got.NRGBAAt(x, y) {
                t.Fatalf("pixel (%d,%d) differs: %v vs %v", x, y, want.NRGBAAt(x, y), got.NRGBAAt(x, y))
            }
        }
    }
}

// TestAutoPaletteShrinksAndLossless builds a low-color image and checks that
// the effort path (which may switch to the palette transform) stays exactly
// lossless and is no larger than -- and here, smaller than -- Method 0.
func TestAutoPaletteShrinksAndLossless(t *testing.T) {
    const w, h = 128, 128
    pal := []color.NRGBA{
        {255, 0, 0, 255}, {0, 255, 0, 255}, {0, 0, 255, 255},
        {255, 255, 0, 255}, {0, 255, 255, 255}, {255, 255, 255, 255},
    }
    orig := image.NewNRGBA(image.Rect(0, 0, w, h))
    for y := 0; y < h; y++ {
        for x := 0; x < w; x++ {
            orig.SetNRGBA(x, y, pal[(x/8+y/8)%len(pal)])
        }
    }

    var m0, m1 bytes.Buffer
    if err := Encode(&m0, orig, &Options{Method: 0}); err != nil {
        t.Fatal(err)
    }
    if err := Encode(&m1, orig, &Options{Method: 1}); err != nil {
        t.Fatal(err)
    }
    t.Logf("method0=%d B, method1=%d B", m0.Len(), m1.Len())

    if m1.Len() > m0.Len() {
        t.Errorf("method1 (%d) larger than method0 (%d); effort must never regress", m1.Len(), m0.Len())
    }
    if m1.Len() >= m0.Len() {
        t.Errorf("auto-palette did not shrink a 6-color image (method1 %d, method0 %d)", m1.Len(), m0.Len())
    }

    // Critical: the palette path must be exactly lossless.
    assertSamePixels(t, orig, decodeNRGBA(t, m1.Bytes()))
}

// TestAutoPaletteSkippedForFullColor makes sure a >256-color image under effort
// still encodes losslessly (the palette candidate is simply not taken).
func TestAutoPaletteSkippedForFullColor(t *testing.T) {
    orig := image.NewNRGBA(image.Rect(0, 0, 64, 64))
    for y := 0; y < 64; y++ {
        for x := 0; x < 64; x++ {
            orig.SetNRGBA(x, y, color.NRGBA{uint8(x * 4), uint8(y * 4), uint8(x*4 ^ y*4), 255})
        }
    }
    if hasFewColors(orig) {
        t.Fatal("test image unexpectedly has <= 256 colors")
    }

    var buf bytes.Buffer
    if err := Encode(&buf, orig, &Options{Method: 1}); err != nil {
        t.Fatal(err)
    }
    assertSamePixels(t, orig, decodeNRGBA(t, buf.Bytes()))
}
