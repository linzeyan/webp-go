package gowebp

import (
    //------------------------------
    //general
    //------------------------------
    "io"
    "bytes"
    "context"
    "runtime"
    "sync"
    "encoding/binary"
    //------------------------------
    //imaging
    //------------------------------
    "image"
    "image/draw"
    "image/color"
    //------------------------------
    //errors
    //------------------------------
    "errors"
    //------------------------------
    //vp8 (lossy)
    //------------------------------
    "github.com/linzeyan/webp-go/internal/vp8enc"
)

// Options holds configuration settings for WebP encoding.
//
// Fields:
//   - UseExtendedFormat: If true, wraps the VP8L frame inside a VP8X container
//     to enable metadata support. This does not affect image compression or
//     encoding itself, as VP8L remains the encoding format. Setting any of
//     ICCProfile/EXIF/XMP also forces the VP8X container automatically.
//   - Lossy: If true, encode as VP8 (lossy). Otherwise (default) encode as
//     VP8L (lossless), preserving existing behavior byte-for-byte.
//   - Quality: Lossy quality in [0, 100]. Higher values preserve more
//     detail. Default (when zero) is 75. Ignored when Lossy is false.
//   - Method: For lossy, the speed/quality tradeoff in [0, 6]; higher values
//     spend more time searching for better compression (default 4). For
//     lossless, Method doubles as an effort knob: 0 keeps the fast default
//     output, while any positive value searches for the smallest predictor
//     tile size and color-cache size (slower, smaller; the decoded pixels are
//     unchanged either way).
//   - ICCProfile: Raw ICC color-profile bytes. When non-empty they are
//     emitted as an ICCP chunk (before the image data) inside a VP8X
//     container. Empty means no profile is written.
//   - EXIF: Raw EXIF metadata bytes, emitted as an EXIF chunk after the
//     image data. Empty means no EXIF chunk is written.
//   - XMP: Raw XMP metadata bytes, emitted as an "XMP " chunk after the
//     image data. Empty means no XMP chunk is written.
//   - NearLossless: Lossless-path only. 0 (default) is exact lossless. A
//     positive value is the maximum number of low bits the encoder may drop
//     from R/G/B in smooth regions (alpha and edges are kept exact), trading a
//     small, bounded per-channel error for smaller files. Sensible range is
//     1..5; larger values lose more. Ignored when Lossy is true.
type Options struct {
    UseExtendedFormat   bool
    Lossy               bool
    Quality             float32
    Method              int
    ICCProfile          []byte
    EXIF                []byte
    XMP                 []byte
    NearLossless        int
}

// Animation holds configuration settings for WebP animations.
//
// It allows encoding a sequence of frames with individual timing and disposal options,
// supporting features like looping and background color settings.
//
// Fields:
//   - Images: A list of frames to be displayed in sequence.
//   - Durations: Timing for each frame in milliseconds, matching the Images slice.
//   - Disposals: Disposal methods for frames after display; 0 = keep, 1 = clear to background.
//   - LoopCount: Number of times the animation should repeat; 0 means infinite looping.
//   - BackgroundColor: Canvas background color in BGRA order, used for clear operations.
type Animation struct {
    Images              []image.Image
    Durations           []uint
    Disposals           []uint
    LoopCount           uint16
    BackgroundColor     uint32
}

// Encode writes the provided image.Image to the specified io.Writer in WebP format.
//
// This function always encodes the image using VP8L (lossless WebP). If `UseExtendedFormat`
// is enabled, it wraps the VP8L frame inside a VP8X container, allowing the use of metadata
// such as EXIF, ICC color profiles, or XMP metadata.
//
// Note: VP8L already supports transparency, so VP8X is **not required** for alpha support.
//
// Parameters:
//   w   - The destination writer where the encoded WebP image will be written.
//   img - The input image to be encoded.
//   o   - Pointer to Options containing encoding settings:
//         - UseExtendedFormat: If true, wraps the image in a VP8X container to enable 
//           extended WebP features like metadata.
//
// Returns:
//   An error if encoding fails or writing to the io.Writer encounters an issue.
func Encode(w io.Writer, img image.Image, o *Options) error {
    return EncodeContext(context.Background(), w, img, o)
}

// EncodeContext behaves like Encode but aborts early, returning ctx.Err(),
// when ctx is cancelled. Cancellation is checked before encoding starts; a
// lossy single-image encode is additionally polled between macroblock rows.
// A lossless single-image encode runs to completion once started (it is fast;
// guard against oversized inputs with an explicit size limit instead).
func EncodeContext(ctx context.Context, w io.Writer, img image.Image, o *Options) error {
    if err := ctx.Err(); err != nil {
        return err
    }
    if o != nil && o.Lossy {
        return encodeLossy(ctx, w, img, o)
    }

    stream, hasAlpha, err := writeBitStream(img, losslessEffort(o), nearLosslessLevel(o))
    if err != nil {
        return err
    }

    meta := optionsMetadata(o)
    buf := &bytes.Buffer{}

    // Chunk order per the WebP container spec: VP8X, ICCP, VP8L, EXIF, XMP.
    if (o != nil && o.UseExtendedFormat) || meta.has() {
        writeChunkVP8X(buf, img.Bounds(), hasAlpha, false, meta)
    }
    writeMetaChunk(buf, "ICCP", meta.icc)

    buf.Write([]byte("VP8L"))
    binary.Write(buf, binary.LittleEndian, uint32(stream.Len()))
    buf.Write(stream.Bytes())
    // RIFF requires even-padded chunk data. The VP8L chunk only needs a pad
    // byte when EXIF/XMP chunks follow it; without trailing metadata the
    // output stays byte-for-byte identical to prior releases.
    if (len(meta.exif) > 0 || len(meta.xmp) > 0) && stream.Len()&1 == 1 {
        buf.WriteByte(0)
    }
    writeMetaChunk(buf, "EXIF", meta.exif)
    writeMetaChunk(buf, "XMP ", meta.xmp)

    w.Write([]byte("RIFF"))
    binary.Write(w, binary.LittleEndian, uint32(4 + buf.Len()))

    w.Write([]byte("WEBP"))
    w.Write(buf.Bytes())

    return nil
}

// encodeLossy produces a VP8-coded .webp file. When the source image has
// alpha, the VP8 (opaque color) chunk is paired with an ALPH chunk
// carrying the alpha plane inside a VP8X container (spec section 2 /
// WebP container spec "Alpha").
func encodeLossy(ctx context.Context, w io.Writer, img image.Image, o *Options) error {
    if o.Quality < 0 || o.Quality > 100 {
        return errors.New("Options.Quality must be in [0, 100]")
    }
    if o.Method < 0 || o.Method > 6 {
        return errors.New("Options.Method must be in [0, 6]")
    }
    q := o.Quality
    if q == 0 {
        q = 75
    }

    meta := optionsMetadata(o)
    alpha := vp8enc.ExtractAlpha(img)
    if alpha == nil && !meta.has() {
        // Fully opaque, no metadata: simple VP8 container, byte-for-byte
        // identical to prior releases.
        return vp8enc.EncodeWebPContext(ctx, w, img, vp8enc.EncodeOptions{
            Quality: q,
            Method:  o.Method,
        })
    }

    // Alpha and/or metadata require the VP8X container. Chunk order per the
    // WebP container spec: VP8X, ICCP, [ALPH], VP8, EXIF, XMP.
    var vp8buf bytes.Buffer
    if err := vp8enc.EncodeFrameContext(ctx, &vp8buf, img, vp8enc.EncodeOptions{
        Quality: q,
        Method:  o.Method,
    }); err != nil {
        return err
    }

    inner := &bytes.Buffer{}
    writeChunkVP8X(inner, img.Bounds(), alpha != nil, false, meta)
    writeMetaChunk(inner, "ICCP", meta.icc)
    if alpha != nil {
        writeChunkALPH(inner, img.Bounds(), alpha)
    }
    writeChunkVP8(inner, vp8buf.Bytes())
    writeMetaChunk(inner, "EXIF", meta.exif)
    writeMetaChunk(inner, "XMP ", meta.xmp)

    w.Write([]byte("RIFF"))
    binary.Write(w, binary.LittleEndian, uint32(4+inner.Len()))
    w.Write([]byte("WEBP"))
    w.Write(inner.Bytes())
    return nil
}

// writeChunkVP8 emits a VP8 sub-chunk with even-length padding.
func writeChunkVP8(buf *bytes.Buffer, payload []byte) {
    buf.Write([]byte("VP8 "))
    binary.Write(buf, binary.LittleEndian, uint32(len(payload)))
    buf.Write(payload)
    if len(payload)&1 == 1 {
        buf.WriteByte(0)
    }
}

// writeChunkALPH emits an ALPH sub-chunk. When VP8L-compressed alpha is
// smaller than raw, we use method=1 (VP8L with alpha stored in the
// green channel); otherwise method=0 (raw alpha plane). Filter and
// preprocessing are always 0 in the current implementation.
//
// Header byte layout is RRMMFFCC where CC is compression method, FF is
// filter, MM is preprocessing, RR is reserved (must be 0).
func writeChunkALPH(buf *bytes.Buffer, bounds image.Rectangle, alpha []byte) {
    body, method := encodeAlphaPayload(alpha, bounds.Dx(), bounds.Dy())
    payloadLen := 1 + len(body)
    buf.Write([]byte("ALPH"))
    binary.Write(buf, binary.LittleEndian, uint32(payloadLen))
    buf.WriteByte(byte(method))
    buf.Write(body)
    if payloadLen&1 == 1 {
        buf.WriteByte(0)
    }
}

// encodeAlphaPayload returns the alpha-plane bytes to embed in an ALPH
// chunk along with the compression-method value (0 or 1) to put in the
// header byte. Tries VP8L first; falls back to raw if it's not smaller.
func encodeAlphaPayload(alpha []byte, w, h int) ([]byte, int) {
    compressed, err := encodeAlphaVP8L(alpha, w, h)
    if err == nil && len(compressed) < len(alpha) {
        return compressed, 1
    }
    return alpha, 0
}

// encodeAlphaVP8L produces a VP8L-compressed alpha plane suitable for
// embedding in an ALPH chunk with compression method 1. The plane is
// encoded as a synthetic NRGBA image whose green channel carries the
// alpha values; the decoder reads only the green channel out of the
// VP8L sub-image.
//
// The 5-byte VP8L header (magic + dimensions + alpha flag + version) is
// stripped from the output because the ALPH decoder synthesizes that
// header from the VP8X-level dimensions.
func encodeAlphaVP8L(alpha []byte, w, h int) ([]byte, error) {
    img := image.NewNRGBA(image.Rect(0, 0, w, h))
    for y := 0; y < h; y++ {
        for x := 0; x < w; x++ {
            img.Pix[(y*w+x)*4+1] = alpha[y*w+x] // green = alpha
            img.Pix[(y*w+x)*4+3] = 0xff         // fully opaque
        }
    }
    stream, _, err := writeBitStream(img, 0, 0)
    if err != nil {
        return nil, err
    }
    b := stream.Bytes()
    if len(b) < 5 {
        return nil, errors.New("VP8L stream too short for alpha payload")
    }
    // Strip the 5-byte VP8L header; the decoder re-synthesizes it from
    // VP8X dimensions when parsing the ALPH chunk.
    return b[5:], nil
}

// EncodeAll writes the provided animation sequence to the specified io.Writer in WebP format.
//
// This function encodes a list of frames as a WebP animation using the VP8X container, which
// supports features like looping, frame timing, disposal methods, and background color settings.
// Each frame is individually compressed using the VP8L (lossless) format.
//
// Note: Even if `UseExtendedFormat` is not explicitly set, animations always use the VP8X container
// because it is required for WebP animation support.
//
// Parameters:
//   w   - The destination writer where the encoded WebP animation will be written.
//   ani - Pointer to Animation containing the frames and animation settings:
//         - Images: List of frames to encode.
//         - Durations: Display times for each frame in milliseconds.
//         - Disposals: Disposal methods after frame display (keep or clear).
//         - LoopCount: Number of times the animation should loop (0 = infinite).
//         - BackgroundColor: Background color for the canvas, used when clearing.
//   o   - Pointer to Options containing additional encoding settings:
//         - UseExtendedFormat: Currently unused for animations, but accepted for consistency.
//
// Returns:
//   An error if encoding fails or writing to the io.Writer encounters an issue.
func EncodeAll(w io.Writer, ani *Animation, o *Options) error {
    return EncodeAllContext(context.Background(), w, ani, o)
}

// EncodeAllContext behaves like EncodeAll but aborts with ctx.Err() when ctx
// is cancelled. Cancellation is checked before each frame is encoded, which
// is the granularity that matters for long multi-frame animations.
func EncodeAllContext(ctx context.Context, w io.Writer, ani *Animation, o *Options) error {
    frames, alpha, err := writeFrames(ctx, ani, o)
    if err != nil {
        return err
    }

    var bounds image.Rectangle
    for _, img := range ani.Images {
        bounds.Max.X = max(img.Bounds().Max.X, bounds.Max.X)
        bounds.Max.Y = max(img.Bounds().Max.Y, bounds.Max.Y)
    }

    meta := optionsMetadata(o)
    buf := &bytes.Buffer{}

    // Chunk order per the WebP container spec: VP8X, ICCP, ANIM+frames,
    // EXIF, XMP.
    writeChunkVP8X(buf, bounds, alpha, true, meta)
    writeMetaChunk(buf, "ICCP", meta.icc)

    buf.Write([]byte("ANIM"))
    binary.Write(buf, binary.LittleEndian, uint32(6))
    binary.Write(buf, binary.LittleEndian, uint32(ani.BackgroundColor))
    binary.Write(buf, binary.LittleEndian, uint16(ani.LoopCount))

    buf.Write(frames.Bytes())

    writeMetaChunk(buf, "EXIF", meta.exif)
    writeMetaChunk(buf, "XMP ", meta.xmp)

    w.Write([]byte("RIFF"))
    binary.Write(w, binary.LittleEndian, uint32(4 + buf.Len()))

    w.Write([]byte("WEBP"))
    w.Write(buf.Bytes())

    return nil
}

// metadata bundles the optional ICC/EXIF/XMP payloads pulled from Options so
// they can be threaded through the chunk writers without widening every
// signature with three more slices.
type metadata struct {
    icc  []byte
    exif []byte
    xmp  []byte
}

// has reports whether any metadata payload is present, i.e. whether the VP8X
// container must be emitted even when the caller did not set UseExtendedFormat.
func (m metadata) has() bool {
    return len(m.icc) > 0 || len(m.exif) > 0 || len(m.xmp) > 0
}

// optionsMetadata extracts the metadata payloads from Options, tolerating a nil
// Options pointer (the common "encode with defaults" call).
func optionsMetadata(o *Options) metadata {
    if o == nil {
        return metadata{}
    }
    return metadata{icc: o.ICCProfile, exif: o.EXIF, xmp: o.XMP}
}

// writeMetaChunk emits a metadata sub-chunk (ICCP/EXIF/XMP) with even-length
// padding. It is a no-op for empty payloads so callers can invoke it
// unconditionally. fourCC must be exactly four bytes (e.g. "XMP " with a
// trailing space).
func writeMetaChunk(buf *bytes.Buffer, fourCC string, payload []byte) {
    if len(payload) == 0 {
        return
    }
    buf.Write([]byte(fourCC))
    binary.Write(buf, binary.LittleEndian, uint32(len(payload)))
    buf.Write(payload)
    if len(payload)&1 == 1 {
        buf.WriteByte(0)
    }
}

func writeChunkVP8X(buf *bytes.Buffer, bounds image.Rectangle, flagAlpha, flagAni bool, meta metadata) {
    buf.Write([]byte("VP8X"))
    binary.Write(buf, binary.LittleEndian, uint32(10))

    // VP8X feature flags byte (WebP container spec): Rsv Rsv I L E X A R.
    var flags byte
    if flagAni {
        flags |= 1 << 1 // A: animation
    }

    if flagAlpha {
        flags |= 1 << 4 // L: alpha
    }

    if len(meta.icc) > 0 {
        flags |= 1 << 5 // I: ICC profile
    }

    if len(meta.exif) > 0 {
        flags |= 1 << 3 // E: EXIF metadata
    }

    if len(meta.xmp) > 0 {
        flags |= 1 << 2 // X: XMP metadata
    }

    binary.Write(buf, binary.LittleEndian, flags)
    buf.Write([]byte{0x00, 0x00, 0x00})

    dx := bounds.Dx() - 1
    dy := bounds.Dy() - 1

    buf.Write([]byte{byte(dx), byte(dx >> 8), byte(dx >> 16)})
    buf.Write([]byte{byte(dy), byte(dy >> 8), byte(dy >> 16)})
}

func writeFrames(ctx context.Context, ani *Animation, o *Options) (*bytes.Buffer, bool, error) {
    if len(ani.Images) == 0 {
        return nil, false, errors.New("must provide at least one image")
    }

    if len(ani.Images) != len(ani.Durations) {
        return nil, false, errors.New("mismatched image and durations lengths")
    }

    if len(ani.Images) != len(ani.Disposals) {
        return nil, false, errors.New("mismatched image and disposals lengths")
    }

    for i := 0; i < len(ani.Images); i++ {
        ani.Durations[i] = min(ani.Durations[i], 1 << 24 - 1)
        ani.Disposals[i] = min(ani.Disposals[i], 1)
    }

    lossy := o != nil && o.Lossy
    quality := float32(75)
    method := 0
    if o != nil && o.Quality != 0 {
        quality = o.Quality
    }
    if o != nil {
        method = o.Method
    }
    nearLossless := nearLosslessLevel(o)

    // Each frame's payload is encoded independently (frame encodes share no
    // mutable state), so they run concurrently. The ANMF stream is then
    // assembled in frame order, leaving the output byte-identical to a serial
    // run.
    n := len(ani.Images)
    payloads := make([][]byte, n)
    alphas := make([]bool, n)
    err := parallelFor(ctx, n, func(i int) error {
        payload, alpha, err := encodeFramePayload(ctx, ani.Images[i], lossy, quality, method, nearLossless)
        if err != nil {
            return err
        }
        payloads[i] = payload
        alphas[i] = alpha
        return nil
    })
    if err != nil {
        return nil, false, err
    }

    buf := &bytes.Buffer{}
    var hasAlpha bool
    for i, img := range ani.Images {
        hasAlpha = hasAlpha || alphas[i]

        w := &bitWriter{Buffer: buf}
        w.writeBytes([]byte("ANMF"))
        // ANMF payload = 16-byte frame header + framePayload (which
        // already contains its own sub-chunk headers + padding).
        w.writeBits(uint64(16+len(payloads[i])), 32)

        // WebP specs requires frame offsets to be divided by 2
        w.writeBits(uint64(img.Bounds().Min.X/2), 24)
        w.writeBits(uint64(img.Bounds().Min.Y/2), 24)

        w.writeBits(uint64(img.Bounds().Dx()-1), 24)
        w.writeBits(uint64(img.Bounds().Dy()-1), 24)

        w.writeBits(uint64(ani.Durations[i]), 24)
        w.writeBits(uint64(ani.Disposals[i]), 1)
        w.writeBits(uint64(0), 1)
        w.writeBits(uint64(0), 6)

        w.Buffer.Write(payloads[i])
    }

    return buf, hasAlpha, nil
}

// encodeFramePayload encodes a single animation frame into the bytes that go
// inside its ANMF chunk (an optional ALPH chunk plus a VP8/VP8L sub-chunk). It
// holds no shared state, so writeFrames can call it from many goroutines.
func encodeFramePayload(ctx context.Context, img image.Image, lossy bool, quality float32, method, nearLossless int) ([]byte, bool, error) {
    var framePayload bytes.Buffer
    var alpha bool

    if lossy {
        // Lossy animation frame: optional ALPH (if image has alpha) followed
        // by a VP8 color chunk, both wrapped in the ANMF.
        alphaPlane := vp8enc.ExtractAlpha(img)
        if alphaPlane != nil {
            writeChunkALPH(&framePayload, img.Bounds(), alphaPlane)
            alpha = true
        }
        var vp8 bytes.Buffer
        if err := vp8enc.EncodeFrameContext(ctx, &vp8, img, vp8enc.EncodeOptions{
            Quality: quality,
            Method:  method,
        }); err != nil {
            return nil, false, err
        }
        writeChunkVP8(&framePayload, vp8.Bytes())
    } else {
        stream, a, err := writeBitStream(img, method, nearLossless)
        if err != nil {
            return nil, false, err
        }
        // VP8L sub-chunk inside ANMF. Use the same fourcc/size/pad pattern as
        // writeChunkVP8 for consistency.
        framePayload.Write([]byte("VP8L"))
        binary.Write(&framePayload, binary.LittleEndian, uint32(stream.Len()))
        framePayload.Write(stream.Bytes())
        if stream.Len()&1 == 1 {
            framePayload.WriteByte(0)
        }
        alpha = a
    }

    return framePayload.Bytes(), alpha, nil
}

// parallelFor runs fn(0..n-1) across up to GOMAXPROCS worker goroutines and
// returns the first error. fn must only touch state indexed by its argument
// (no cross-index sharing), which keeps the calls data-race free. ctx is polled
// before each item so a cancelled context aborts the remaining frames; a single
// item runs inline without spawning goroutines.
func parallelFor(ctx context.Context, n int, fn func(i int) error) error {
    if n <= 0 {
        return nil
    }
    if n == 1 {
        if err := ctx.Err(); err != nil {
            return err
        }
        return fn(0)
    }

    workers := runtime.GOMAXPROCS(0)
    if workers > n {
        workers = n
    }

    errs := make([]error, n)
    idx := make(chan int)
    var wg sync.WaitGroup
    for w := 0; w < workers; w++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            for i := range idx {
                if err := ctx.Err(); err != nil {
                    errs[i] = err
                    continue
                }
                errs[i] = fn(i)
            }
        }()
    }
    for i := 0; i < n; i++ {
        idx <- i
    }
    close(idx)
    wg.Wait()

    for _, err := range errs {
        if err != nil {
            return err
        }
    }
    return nil
}

// losslessEffort maps Options to the VP8L encoder effort. 0 (the default and
// the nil-Options case) keeps the fast, historically-stable output; a positive
// value opts into the color-cache search in writeBitStreamData. Method is
// reused as the lossless effort knob (it otherwise only affects lossy).
func losslessEffort(o *Options) int {
    if o == nil {
        return 0
    }
    return o.Method
}

func nearLosslessLevel(o *Options) int {
    if o == nil {
        return 0
    }
    return o.NearLossless
}

func writeBitStream(img image.Image, effort, nearLossless int) (*bytes.Buffer, bool, error) {
    if img == nil {
        return nil, false, errors.New("image is nil")
    }

    if img.Bounds().Dx() < 1 || img.Bounds().Dy() < 1 {
        return nil, false, errors.New("invalid image size")
    }

    if img.Bounds().Dx() > 1 << 14 || img.Bounds().Dy() > 1 << 14 {
        return nil, false, errors.New("invalid image size")
    }

    _, isIndexed := img.(*image.Paletted)

    // rgba is the working copy of the source. draw.Draw with Src overwrites
    // every byte, so its Pix can come from the pool uncleared.
    rw, rh := img.Bounds().Dx(), img.Bounds().Dy()
    pixBuf := getByteBuf(rw * rh * 4)
    defer putByteBuf(pixBuf)
    rgba := &image.NRGBA{Pix: *pixBuf, Stride: rw * 4, Rect: image.Rect(0, 0, rw, rh)}
    draw.Draw(rgba, rgba.Bounds(), img, img.Bounds().Min, draw.Src)

    var transforms [4]bool
    transforms[transformPredict] = !isIndexed
    transforms[transformColor] = false
    transforms[transformSubGreen] = !isIndexed
    transforms[transformColorIndexing] = isIndexed

    data, err := encodeData(rgba, transforms, effort)
    if err != nil {
        return nil, false, err
    }

    // Near-lossless: also encode a precision-reduced copy and keep whichever
    // is smaller. Discretization shrinks noisy/photographic content but can
    // grow clean synthetic gradients, so the fallback guarantees the output is
    // never larger than plain lossless while honoring the error bound.
    if nearLossless > 0 && !isIndexed {
        nb := getByteBuf(len(rgba.Pix))
        near := &image.NRGBA{Pix: (*nb)[:len(rgba.Pix)], Stride: rgba.Stride, Rect: rgba.Rect}
        copy(near.Pix, rgba.Pix)
        applyNearLossless(near, nearLossless)
        nearData, err := encodeData(near, transforms, effort)
        putByteBuf(nb)
        if err != nil {
            return nil, false, err
        }
        if nearData.Len() < data.Len() {
            data = nearData
        }
    }

    // Auto-palette: a non-indexed image with <= 256 distinct colors (logos,
    // screenshots, line art) often packs far smaller via the color-indexing
    // transform. Try it under effort and keep it only when it wins.
    if effort > 0 && !isIndexed && hasFewColors(rgba) {
        var palTransforms [4]bool
        palTransforms[transformColorIndexing] = true
        if palData, err := encodeData(rgba, palTransforms, effort); err == nil && palData.Len() < data.Len() {
            data = palData
        }
    }

    b := &bytes.Buffer{}
    s := &bitWriter{Buffer: b}

    // The bit-stream header is exactly 40 bits (a whole number of bytes), so s
    // is byte-aligned here and the data block's bytes can be spliced in
    // directly. Alpha is untouched by near-lossless, so the flag is stable.
    writeBitStreamHeader(s, rgba.Bounds(), !rgba.Opaque())
    s.writeBytes(data.Bytes())

    s.alignByte()

    if b.Len() % 2 != 0 {
        b.Write([]byte{0x00})
    }

    return b, !rgba.Opaque(), nil
}

// hasFewColors reports whether img has at most 256 distinct RGBA colors, the
// precondition for the color-indexing (palette) transform. It bails out as
// soon as a 257th color appears, so it is cheap on full-color images.
func hasFewColors(img *image.NRGBA) bool {
    seen := make(map[uint32]struct{}, 257)
    for i := 0; i+4 <= len(img.Pix); i += 4 {
        key := uint32(img.Pix[i])<<24 | uint32(img.Pix[i+1])<<16 | uint32(img.Pix[i+2])<<8 | uint32(img.Pix[i+3])
        if _, ok := seen[key]; !ok {
            seen[key] = struct{}{}
            if len(seen) > 256 {
                return false
            }
        }
    }
    return true
}

// defaultPredictBits is the predictor-transform tile size (in bits; 1<<4 = 16
// px tiles) used by the fast path and as the historical default.
const defaultPredictBits = 4

// predictBitsCandidates are the predictor-transform tile sizes the effort
// search tries (2..6 → 4..64 px tiles). Smaller tiles adapt the predictor more
// finely but cost more block-image overhead; the best tradeoff is image-
// dependent, so we measure real encoded size for each.
var predictBitsCandidates = []int{2, 3, 4, 5, 6}

// encodeData produces the VP8L data block (everything after the bit-stream
// header) into a fresh byte-aligned buffer, running the effort search when
// effort > 0. Splitting this out lets writeBitStream encode several candidate
// pixel buffers (e.g. exact vs near-lossless) and keep the smallest.
func encodeData(img image.Image, transforms [4]bool, effort int) (*bytes.Buffer, error) {
    if effort <= 0 {
        return buildBitStreamData(img, transforms, 0, defaultPredictBits)
    }
    return searchBestData(img, transforms, effort)
}

// searchBestData builds the VP8L data block under a non-zero effort: it picks
// the predictor tile size that encodes smallest, then does the final build at
// that size with the color-cache search enabled. Because defaultPredictBits is
// among the candidates and the color-cache search includes the default size,
// the result is never larger than the effort-0 output.
func searchBestData(img image.Image, transforms [4]bool, effort int) (*bytes.Buffer, error) {
    bestBits := defaultPredictBits
    if transforms[transformPredict] {
        bestLen := -1
        for _, pb := range predictBitsCandidates {
            // Tile-size selection uses the fixed default cache (effort 0) so
            // the candidates are compared cheaply and fairly.
            buf, err := buildBitStreamData(img, transforms, 0, pb)
            if err != nil {
                return nil, err
            }
            if bestLen < 0 || buf.Len() < bestLen {
                bestLen, bestBits = buf.Len(), pb
            }
        }
    }
    return buildBitStreamData(img, transforms, effort, bestBits)
}

// buildBitStreamData encodes one VP8L data block into a fresh, byte-aligned
// buffer so the caller can measure its size or splice it into the stream.
func buildBitStreamData(img image.Image, transforms [4]bool, effort, predictBits int) (*bytes.Buffer, error) {
    buf := &bytes.Buffer{}
    bw := &bitWriter{Buffer: buf}
    if err := writeBitStreamData(bw, img, 4, effort, predictBits, transforms); err != nil {
        return nil, err
    }
    bw.alignByte()
    return buf, nil
}

func writeBitStreamHeader(w *bitWriter, bounds image.Rectangle, hasAlpha bool) {
    w.writeBits(0x2f, 8)

    w.writeBits(uint64(bounds.Dx() - 1), 14)
    w.writeBits(uint64(bounds.Dy() - 1), 14)

    if hasAlpha {
        w.writeBits(1, 1)
    } else {
        w.writeBits(0, 1)
    }

    w.writeBits(0, 3)
}

func writeBitStreamData(w *bitWriter, img image.Image, colorCacheBits, effort, predictBits int, transforms [4]bool) error {
    width := img.Bounds().Dx()
    height := img.Bounds().Dy()

    // pixels is the mutable working copy of the image. Recycle its backing
    // buffer through the pool; the palette transform may rebind pixels to a
    // narrower packed buffer, but pbuf still references the original, so it is
    // released correctly at return.
    pbuf := getPixelBuf(width * height)
    defer putPixelBuf(pbuf)
    pixels := *pbuf
    if err := flattenInto(img, pixels); err != nil {
        return err
    }

    if transforms[transformColorIndexing] {
        w.writeBits(1, 1)
        w.writeBits(3, 2)
       
        pal, pw, err := applyPaletteTransform(&pixels, width, height)
        if err != nil {
            return err
        }

        width = pw
       
        w.writeBits(uint64(len(pal) - 1), 8);
        writeImageData(w, pal, len(pal), 1, false, colorCacheBits);
    }

    if transforms[transformSubGreen] {
        w.writeBits(1, 1)
        w.writeBits(2, 2)

        applySubtractGreenTransform(pixels)
    }

    if transforms[transformColor] {
        w.writeBits(1, 1)
        w.writeBits(1, 2)

        bits, bw, bh, blocks := applyColorTransform(pixels, width, height)

        w.writeBits(uint64(bits - 2), 3);
        writeImageData(w, blocks, bw, bh, false, colorCacheBits)
    }

    if transforms[transformPredict] {
        w.writeBits(1, 1)
        w.writeBits(0, 2)

        bits, bw, bh, blocks := applyPredictTransform(pixels, width, height, predictBits)

        w.writeBits(uint64(bits - 2), 3);
        writeImageData(w, blocks, bw, bh, false, colorCacheBits)
    }

    w.writeBits(0, 1) // end of transform

    // The main image data dominates the output. With a non-zero effort,
    // search for the color-cache size that encodes it smallest instead of
    // using the fixed default; the result is byte-different but always
    // losslessly decodable. effort 0 preserves the historical output.
    mainCacheBits := colorCacheBits
    if effort > 0 {
        mainCacheBits = bestMainCacheBits(pixels, width, height)
    }
    writeImageData(w, pixels, width, height, true, mainCacheBits)

    return nil
}

// maxLosslessCacheBits is the largest VP8L color-cache size (in bits) the
// search considers. The spec allows 1..11; 0 means no cache.
const maxLosslessCacheBits = 11

// bestMainCacheBits returns the color-cache size (in bits, 0 = no cache) that
// encodes the main image data smallest. It trial-encodes the pixels with each
// candidate size into a throwaway bit writer and keeps the cheapest. Because
// VP8L is lossless, the choice only affects size, never the decoded pixels.
func bestMainCacheBits(pixels []color.NRGBA, width, height int) int {
    best, bestBits := 0, -1
    for cb := 0; cb <= maxLosslessCacheBits; cb++ {
        scratch := &bitWriter{Buffer: &bytes.Buffer{}}
        writeImageData(scratch, pixels, width, height, true, cb)
        bits := scratch.Buffer.Len()*8 + scratch.BitBufferSize
        if bestBits < 0 || bits < bestBits {
            best, bestBits = cb, bits
        }
    }
    return best
}

func writeImageData(w *bitWriter, pixels []color.NRGBA, width, height int, isRecursive bool, colorCacheBits int) {
    if colorCacheBits > 0 {
        w.writeBits(1, 1)
        w.writeBits(uint64(colorCacheBits), 4) 
    } else {
        w.writeBits(0, 1)
    }

    if isRecursive {
        w.writeBits(0, 1)
    }

    scratch := getLZ77Scratch(len(pixels))
    encoded := encodeImageData(pixels, width, height, colorCacheBits, scratch)
    histos := computeHistograms(encoded, colorCacheBits)

    var codes [][]huffmanCode
    for i := 0; i < 5; i++ {
        // WebP specs requires Huffman codes with maximum depth of 15
        c := buildhuffmanCodes(histos[i], 15)
        codes = append(codes, c)

        writehuffmanCodes(w, c)
    }

    for i := 0; i < len(encoded); i ++ {
        w.writeCode(codes[0][encoded[i + 0]])
        if encoded[i + 0] < 256 {
            w.writeCode(codes[1][encoded[i + 1]])
            w.writeCode(codes[2][encoded[i + 2]])
            w.writeCode(codes[3][encoded[i + 3]])
            i += 3
        } else if encoded[i + 0] < 256 + 24 {
            cnt := prefixEncodeBits(int(encoded[i + 0]) - 256)
            w.writeBits(uint64(encoded[i + 1]), cnt);

            w.writeCode(codes[4][encoded[i + 2]])

            cnt = prefixEncodeBits(int(encoded[i + 2]))
            w.writeBits(uint64(encoded[i + 3]), cnt);
            i += 3
        }
    }

    // encoded aliases scratch.encoded and is no longer read past this point.
    lz77Pool.Put(scratch)
}

// lz77Scratch holds the large per-call buffers of encodeImageData so they can
// be reused across the many calls an encode (and especially the effort search)
// makes, instead of being reallocated each time.
type lz77Scratch struct {
    head    []int // 1<<14 hash heads; must be zeroed per use
    prev    []int // sized to len(pixels)
    encoded []int // sized to len(pixels)*4
}

var lz77Pool = sync.Pool{
    New: func() any { return &lz77Scratch{head: make([]int, 1<<14)} },
}

// pixelBufPool recycles full-image []color.NRGBA scratch buffers (e.g. the
// predictor transform's residual plane). A *slice is stored so Put doesn't
// allocate boxing the slice header.
var pixelBufPool = sync.Pool{
    New: func() any { s := []color.NRGBA(nil); return &s },
}

// getPixelBuf returns a buffer of length n from the pool, growing it if needed.
// The contents are arbitrary, so the caller must fully write before reading.
func getPixelBuf(n int) *[]color.NRGBA {
    p := pixelBufPool.Get().(*[]color.NRGBA)
    if cap(*p) < n {
        *p = make([]color.NRGBA, n)
    } else {
        *p = (*p)[:n]
    }
    return p
}

func putPixelBuf(p *[]color.NRGBA) { pixelBufPool.Put(p) }

// bytePool recycles the Pix byte buffers of the working *image.NRGBA images.
var bytePool = sync.Pool{
    New: func() any { s := []byte(nil); return &s },
}

// getByteBuf returns a byte buffer of length n. The caller must overwrite it
// fully before reading (draw.Draw / copy both do).
func getByteBuf(n int) *[]byte {
    p := bytePool.Get().(*[]byte)
    if cap(*p) < n {
        *p = make([]byte, n)
    } else {
        *p = (*p)[:n]
    }
    return p
}

func putByteBuf(p *[]byte) { bytePool.Put(p) }

// getLZ77Scratch returns a scratch whose buffers fit npix pixels. head is
// cleared because the LZ77 hash table must start empty; prev and encoded are
// always written before being read within a call, so they need no clearing.
func getLZ77Scratch(npix int) *lz77Scratch {
    s := lz77Pool.Get().(*lz77Scratch)
    clear(s.head)
    if cap(s.prev) < npix {
        s.prev = make([]int, npix)
    } else {
        s.prev = s.prev[:npix]
    }
    if need := npix * 4; cap(s.encoded) < need {
        s.encoded = make([]int, need)
    } else {
        s.encoded = s.encoded[:need]
    }
    return s
}

func encodeImageData(pixels []color.NRGBA, width, height, colorCacheBits int, scratch *lz77Scratch) []int {
    head := scratch.head
    prev := scratch.prev
    cache := make([]color.NRGBA, 1 << colorCacheBits)

    encoded := scratch.encoded
    cnt := 0

    var distances = []int {
        96,   73,  55,  39,  23,  13,   5,  1,  255, 255, 255, 255, 255, 255, 255, 255,
        101,  78,  58,  42,  26,  16,   8,  2,    0,   3,  9,   17,  27,  43,  59,  79,
        102,  86,  62,  46,  32,  20,  10,  6,    4,   7,  11,  21,  33,  47,  63,  87,
        105,  90,  70,  52,  37,  28,  18,  14,  12,  15,  19,  29,  38,  53,  71,  91,
        110,  99,  82,  66,  48,  35,  30,  24,  22,  25,  31,  36,  49,  67,  83, 100,
        115, 108,  94,  76,  64,  50,  44,  40,  34,  41,  45,  51,  65,  77,  95, 109,
        118, 113, 103,  92,  80,  68,  60,  56,  54,  57,  61,  69,  81,  93, 104, 114,
        119, 116, 111, 106,  97,  88,  84,  74,  72,  75,  85,  89,  98, 107, 112, 117,
    }

    for i := 0; i < len(pixels); i++ {
        if i + 2 < len(pixels) {
            h := hash(pixels[i + 0], 14)
            h ^= hash(pixels[i + 1], 14) * 0x9e3779b9
            h ^= hash(pixels[i + 2], 14) * 0x85ebca6b
            h = h % (1 << 14)

            cur := head[h] - 1
            prev[i] = head[h]
            head[h] = i + 1

            dis := 0
            streak := 0
            for j := 0; j < 8; j++ {
                // 1 << 20: sliding window size is 2^20 (1,048,576) per WebP specs.
                // 120: reserved margin for offset adjustments.
                if cur == -1 || i - cur >= 1 << 20 - 120 {
                    break
                }

                l := 0
                // Limit the maximum match length to 4096 pixels per WebP specs.
                for i + l < len(pixels) && l < 4096 {
                    if pixels[i + l] != pixels[cur + l] {
                        break
                    }
                    l++
                }

                if l > streak {
                    streak = l
                    dis = i - cur
                }

                cur = prev[cur] - 1
            }

            // Only use the match if it is at least 3 pixels long per WebP specs.
            if streak >= 3 {
                for j := 0; j < streak; j++ {
                    h := hash(pixels[i + j], colorCacheBits)
                    cache[h] = pixels[i + j]
                }
                
                y := dis / width
                x := dis - y * width
            
                code := dis + 120
                if x <= 8 && y < 8 {
                    code = distances[y * 16 + 8 - x] + 1
                } else if x > width - 8 && y < 7 {
                    code = distances[(y + 1) * 16 + 8 + (width - x)] + 1
                }

                s, l := prefixEncodeCode(streak)
                encoded[cnt + 0] = int(s + 256)
                encoded[cnt + 1] = int(l)

                s, l = prefixEncodeCode(code)
                encoded[cnt + 2] = int(s)
                encoded[cnt + 3] = int(l)
                cnt += 4
    
                i += streak - 1
                continue
            }
        }

        p := pixels[i]
        if colorCacheBits > 0 {
            hash := hash(p, colorCacheBits)

            if i > 0 && cache[hash] == p {
                encoded[cnt] = int(hash + 256 + 24)
                cnt++
                continue
            }

            cache[hash] = p
        }

        encoded[cnt+0] = int(p.G)
        encoded[cnt+1] = int(p.R)
        encoded[cnt+2] = int(p.B)
        encoded[cnt+3] = int(p.A)
        cnt += 4
    }

    return encoded[:cnt]
}

func prefixEncodeCode(n int) (int, int) {
    if n <= 5 {
        return max(0, n - 1), 0
    }

    shift := 0
    rem := n - 1
    for rem > 3 {
        rem >>= 1
        shift += 1
    }

    if rem == 2 {
        return 2 + 2 * shift, n - (2 << shift) - 1
    }

    return 3 + 2 * shift, n - (3 << shift) - 1
}

func prefixEncodeBits(prefix int) int {
    if prefix < 4 {
        return 0
    }

    return (prefix - 2) >> 1
}

func hash(c color.NRGBA, shifts int) uint32 {
    //hash formula including magic number 0x1e35a7bd comes directly from WebP specs!
    x := uint32(c.A) << 24 | uint32(c.R) << 16 | uint32(c.G) << 8 | uint32(c.B)
    return (x * 0x1e35a7bd) >> (32 - min(shifts, 32))
}

func computeHistograms(pixels []int, colorCacheBits int) [][]int {
    c := 0
    if colorCacheBits > 0 {
        c = 1 << colorCacheBits
    }

    histos := [][]int{
        make([]int, 256 + 24 + c),
        make([]int, 256),
        make([]int, 256),
        make([]int, 256),
        make([]int, 40),
    }

    for i := 0; i < len(pixels); i++ {
        histos[0][pixels[i]]++
        if(pixels[i] < 256) {
            histos[1][pixels[i + 1]]++
            histos[2][pixels[i + 2]]++
            histos[3][pixels[i + 3]]++
            i += 3
        } else if pixels[i] < 256 + 24 {
            histos[4][pixels[i + 2]]++
            i += 3
        }
    }

    return histos
}

func flatten(img image.Image) ([]color.NRGBA, error) {
    w := img.Bounds().Dx()
    h := img.Bounds().Dy()
    pixels := make([]color.NRGBA, w*h)
    if err := flattenInto(img, pixels); err != nil {
        return nil, err
    }
    return pixels, nil
}

// flattenInto writes img's pixels into dst (which must have length width*height)
// so callers can supply a pooled buffer instead of allocating one per call.
func flattenInto(img image.Image, dst []color.NRGBA) error {
    w := img.Bounds().Dx()
    h := img.Bounds().Dy()

    rgba, ok := img.(*image.NRGBA)
    if !ok {
        return errors.New("unsupported image format")
    }

    for y := 0; y < h; y++ {
        for x := 0; x < w; x++ {
            i := rgba.PixOffset(x, y)
            s := rgba.Pix[i : i + 4 : i + 4]

            dst[y * w + x].R = uint8(s[0])
            dst[y * w + x].G = uint8(s[1])
            dst[y * w + x].B = uint8(s[2])
            dst[y * w + x].A = uint8(s[3])
        }
    }

    return nil
}