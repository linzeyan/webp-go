package gowebp

import (
    "bytes"
    "image"
    "testing"
)

func encodeLossless(t *testing.T, img image.Image, method int) []byte {
    t.Helper()
    var buf bytes.Buffer
    if err := Encode(&buf, img, &Options{Method: method}); err != nil {
        t.Fatalf("Encode method=%d: %v", method, err)
    }
    return buf.Bytes()
}

// decodePixels returns the decoded bounds and a flat RGBA byte slice so two
// encodings can be compared pixel-exact.
func decodePixels(t *testing.T, data []byte) (image.Rectangle, []byte) {
    t.Helper()
    img, err := Decode(bytes.NewReader(data))
    if err != nil {
        t.Fatalf("Decode: %v", err)
    }
    b := img.Bounds()
    px := make([]byte, 0, b.Dx()*b.Dy()*4)
    for y := b.Min.Y; y < b.Max.Y; y++ {
        for x := b.Min.X; x < b.Max.X; x++ {
            r, g, bl, a := img.At(x, y).RGBA()
            px = append(px, byte(r>>8), byte(g>>8), byte(bl>>8), byte(a>>8))
        }
    }
    return b, px
}

// TestLosslessEffortShrinksAndStaysLossless checks that a positive Method on
// the lossless path never produces a larger file than the Method 0 default
// (the color-cache search includes the default size, so it cannot regress) and
// that the two outputs decode to identical pixels.
func TestLosslessEffortShrinksAndStaysLossless(t *testing.T) {
    for _, dim := range []int{32, 64, 128} {
        img := generateTestImageNRGBA(dim, dim, 1.0, false)

        d0 := encodeLossless(t, img, 0)
        d1 := encodeLossless(t, img, 1)

        delta := len(d1) - len(d0)
        pct := 100 * float64(delta) / float64(len(d0))
        t.Logf("%dx%d: method0=%d B, method1=%d B (%+d B, %+.1f%%)", dim, dim, len(d0), len(d1), delta, pct)

        if len(d1) > len(d0) {
            t.Errorf("%dx%d: method1 (%d) larger than method0 (%d); search must never regress size",
                dim, dim, len(d1), len(d0))
        }

        b0, p0 := decodePixels(t, d0)
        b1, p1 := decodePixels(t, d1)
        if b0 != b1 || !bytes.Equal(p0, p1) {
            t.Errorf("%dx%d: method0 and method1 decode to different pixels (not lossless)", dim, dim)
        }
    }
}

// TestLosslessEffortDefaultUnchanged pins that Method 0 and a nil Options
// produce byte-identical lossless output (the historical fast path).
func TestLosslessEffortDefaultUnchanged(t *testing.T) {
    img := generateTestImageNRGBA(40, 40, 1.0, true)

    var nilBuf, m0Buf bytes.Buffer
    if err := Encode(&nilBuf, img, nil); err != nil {
        t.Fatal(err)
    }
    if err := Encode(&m0Buf, img, &Options{Method: 0}); err != nil {
        t.Fatal(err)
    }
    if !bytes.Equal(nilBuf.Bytes(), m0Buf.Bytes()) {
        t.Errorf("nil Options and Method 0 differ (%d vs %d bytes)", nilBuf.Len(), m0Buf.Len())
    }
}
