package gowebp

import (
    "bytes"
    "image"
    "image/color"
    "image/draw"
    "testing"
)

func encodeLen(t *testing.T, img image.Image, o *Options) int {
    t.Helper()
    var buf bytes.Buffer
    if err := Encode(&buf, img, o); err != nil {
        t.Fatal(err)
    }
    return buf.Len()
}

// maxChannelError encodes img with the given options, decodes the result, and
// returns the largest per-channel RGB difference from the original; it also
// asserts alpha is preserved exactly.
func maxChannelError(t *testing.T, orig *image.NRGBA, o *Options) int {
    t.Helper()
    var buf bytes.Buffer
    if err := Encode(&buf, orig, o); err != nil {
        t.Fatal(err)
    }
    decoded, err := Decode(bytes.NewReader(buf.Bytes()))
    if err != nil {
        t.Fatal(err)
    }
    dec := image.NewNRGBA(decoded.Bounds())
    draw.Draw(dec, dec.Bounds(), decoded, decoded.Bounds().Min, draw.Src)

    b := orig.Bounds()
    maxErr := 0
    for y := 0; y < b.Dy(); y++ {
        for x := 0; x < b.Dx(); x++ {
            oc, dc := orig.NRGBAAt(x, y), dec.NRGBAAt(x, y)
            for _, e := range []int{absDiff(int(oc.R), int(dc.R)), absDiff(int(oc.G), int(dc.G)), absDiff(int(oc.B), int(dc.B))} {
                if e > maxErr {
                    maxErr = e
                }
            }
            if oc.A != dc.A {
                t.Fatalf("alpha changed at (%d,%d): %d -> %d", x, y, oc.A, dc.A)
            }
        }
    }
    return maxErr
}

// TestNearLosslessShrinksNoisy uses a flat region with low-amplitude noise —
// the realistic case (sensor noise in a flat photo area) — and checks that
// near-lossless both shrinks the file and stays within the error bound.
func TestNearLosslessShrinksNoisy(t *testing.T) {
    const w, h, bits = 192, 192, 3

    orig := image.NewNRGBA(image.Rect(0, 0, w, h))
    for y := 0; y < h; y++ {
        for x := 0; x < w; x++ {
            seed := x*1103515245 + y*12345
            r := uint8(128 + (seed%7 - 3))        // 125..131
            g := uint8(128 + ((seed/7)%7 - 3))    // independent per channel
            b := uint8(128 + ((seed/49)%7 - 3))
            orig.SetNRGBA(x, y, color.NRGBA{r, g, b, 255})
        }
    }

    lossless := encodeLen(t, orig, nil)
    near := encodeLen(t, orig, &Options{NearLossless: bits})
    t.Logf("lossless=%d B, near-lossless(bits=%d)=%d B", lossless, bits, near)

    if near >= lossless {
        t.Errorf("near-lossless (%d) did not shrink noisy content (lossless %d)", near, lossless)
    }

    limit := (1 << bits) - 1
    if e := maxChannelError(t, orig, &Options{NearLossless: bits}); e == 0 || e > limit {
        t.Errorf("per-channel error %d, want in (0, %d]", e, limit)
    }
}

// TestNearLosslessNeverLarger uses a clean gradient, where discretization would
// hurt; the exact/near fallback must keep the output no larger than lossless.
func TestNearLosslessNeverLarger(t *testing.T) {
    const w, h = 96, 96

    orig := image.NewNRGBA(image.Rect(0, 0, w, h))
    for y := 0; y < h; y++ {
        for x := 0; x < w; x++ {
            orig.SetNRGBA(x, y, color.NRGBA{uint8(x * 2), uint8(y * 2), uint8(x + y), 255})
        }
    }

    lossless := encodeLen(t, orig, nil)
    for _, bits := range []int{1, 3, 5} {
        if near := encodeLen(t, orig, &Options{NearLossless: bits}); near > lossless {
            t.Errorf("bits=%d: near-lossless (%d) larger than lossless (%d)", bits, near, lossless)
        }
    }
}

// TestNearLosslessDefaultUnchanged pins that NearLossless 0 leaves the lossless
// output byte-for-byte identical to nil Options.
func TestNearLosslessDefaultUnchanged(t *testing.T) {
    img := generateTestImageNRGBA(40, 40, 1.0, true)

    var a, b bytes.Buffer
    if err := Encode(&a, img, nil); err != nil {
        t.Fatal(err)
    }
    if err := Encode(&b, img, &Options{NearLossless: 0}); err != nil {
        t.Fatal(err)
    }
    if !bytes.Equal(a.Bytes(), b.Bytes()) {
        t.Errorf("NearLossless 0 differs from nil Options (%d vs %d bytes)", a.Len(), b.Len())
    }
}
