package gowebp

import "image"

// applyNearLossless reduces the precision of R/G/B in smooth regions of img,
// in place, trading a small bounded error for better compressibility. It is a
// preprocessing pass for the lossless (VP8L) encoder.
//
// bits is the maximum number of low bits that may be dropped. A channel value
// is discretized only where the pixel is interior and all four of its 4-
// neighbors are within 2^bits of it in that channel, so edges keep full
// precision and no banding is introduced beyond the bounded step. The alpha
// channel is always left exact (otherwise a uniformly opaque image would drift
// to A=254). bits <= 0 is a no-op.
//
// The resulting per-channel error to the original is at most 2^bits - 1 (and
// 2^(bits-1) away from clamping boundaries); the decode stays exact relative to
// the discretized pixels, so the output is still a valid lossless WebP of the
// preprocessed image.
func applyNearLossless(img *image.NRGBA, bits int) {
    if bits <= 0 {
        return
    }

    b := img.Bounds()
    w, h := b.Dx(), b.Dy()
    if w < 3 || h < 3 {
        return // no interior pixels to touch
    }

    // Read neighbor values from an unmodified copy so discretization of one
    // pixel cannot feed into the smoothness test of the next.
    src := make([]byte, len(img.Pix))
    copy(src, img.Pix)

    thr := 1 << bits

    for y := 1; y < h-1; y++ {
        for x := 1; x < w-1; x++ {
            off := y*img.Stride + x*4
            for c := 0; c < 3; c++ { // R, G, B only; alpha kept exact
                v := int(src[off+c])
                l := int(src[off-4+c])
                r := int(src[off+4+c])
                t := int(src[off-img.Stride+c])
                d := int(src[off+img.Stride+c])
                if absDiff(v, l) < thr && absDiff(v, r) < thr &&
                    absDiff(v, t) < thr && absDiff(v, d) < thr {
                    img.Pix[off+c] = discretizeChannel(uint8(v), bits)
                }
            }
        }
    }
}

func absDiff(a, b int) int {
    if a < b {
        return b - a
    }
    return a - b
}

// discretizeChannel rounds v to the nearest multiple of 2^bits, clamped to
// [0, 255]. The error is at most 2^(bits-1), or up to 2^bits-1 next to the 255
// boundary where the nearest multiple would overflow.
func discretizeChannel(v uint8, bits int) uint8 {
    mask := (1 << bits) - 1
    n := (int(v) + (1 << (bits - 1))) &^ mask
    if n > 0xff {
        n = 0xff &^ mask
    }
    return uint8(n)
}
