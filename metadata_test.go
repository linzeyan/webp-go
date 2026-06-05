package gowebp

import (
    "bytes"
    "encoding/binary"
    "image"
    "reflect"
    "testing"
)

// webpChunk is a parsed RIFF sub-chunk: its 4-byte FourCC id and the payload
// with any even-length padding byte stripped.
type webpChunk struct {
    id      string
    payload []byte
}

// parseWebPChunks walks the RIFF container and returns its sub-chunks in file
// order. It also asserts the RIFF size field matches the file length, which
// transitively verifies every chunk (including the last) is correctly
// even-padded.
func parseWebPChunks(t *testing.T, data []byte) []webpChunk {
    t.Helper()
    if len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
        t.Fatalf("not a RIFF/WEBP file")
    }
    if riffSize := binary.LittleEndian.Uint32(data[4:8]); int(riffSize)+8 != len(data) {
        t.Fatalf("RIFF size %d + 8 != file length %d (padding bug?)", riffSize, len(data))
    }

    var chunks []webpChunk
    for i := 12; i+8 <= len(data); {
        id := string(data[i : i+4])
        size := int(binary.LittleEndian.Uint32(data[i+4 : i+8]))
        start := i + 8
        if start+size > len(data) {
            t.Fatalf("chunk %q size %d overruns file", id, size)
        }
        chunks = append(chunks, webpChunk{id: id, payload: data[start : start+size]})
        i = start + size
        if size&1 == 1 {
            i++ // RIFF chunks are padded to even length.
        }
    }
    return chunks
}

func chunkIDs(chunks []webpChunk) []string {
    ids := make([]string, len(chunks))
    for i, c := range chunks {
        ids[i] = c.id
    }
    return ids
}

func assertChunkPayload(t *testing.T, chunks []webpChunk, id string, want []byte) {
    t.Helper()
    for _, c := range chunks {
        if c.id == id {
            if !bytes.Equal(c.payload, want) {
                t.Errorf("chunk %q payload = %v, want %v", id, c.payload, want)
            }
            return
        }
    }
    t.Errorf("chunk %q not found", id)
}

// Distinctive metadata payloads. ICC and XMP are deliberately odd-length to
// exercise the even-padding logic in writeMetaChunk and the VP8L chunk.
var (
    sampleICC  = []byte{0x01, 0x02, 0x03}                   // odd length
    sampleEXIF = []byte("Exif\x00\x00MM\x00")               // even length
    sampleXMP  = []byte("<x:xmpmeta>hello</x:xmpmeta>")     // odd length
)

func TestEncodeMetadataLossless(t *testing.T) {
    img := generateTestImageNRGBA(16, 16, 1.0, false) // opaque

    var buf bytes.Buffer
    if err := Encode(&buf, img, &Options{
        ICCProfile: sampleICC,
        EXIF:       sampleEXIF,
        XMP:        sampleXMP,
    }); err != nil {
        t.Fatal(err)
    }

    chunks := parseWebPChunks(t, buf.Bytes())
    want := []string{"VP8X", "ICCP", "VP8L", "EXIF", "XMP "}
    if got := chunkIDs(chunks); !reflect.DeepEqual(got, want) {
        t.Fatalf("chunk order = %v, want %v", got, want)
    }

    if f := chunks[0].payload[0]; f&0x20 == 0 || f&0x08 == 0 || f&0x04 == 0 {
        t.Errorf("VP8X flags = %#x, want ICC(0x20)|EXIF(0x08)|XMP(0x04) bits set", f)
    }

    assertChunkPayload(t, chunks, "ICCP", sampleICC)
    assertChunkPayload(t, chunks, "EXIF", sampleEXIF)
    assertChunkPayload(t, chunks, "XMP ", sampleXMP)

    if _, err := Decode(bytes.NewReader(buf.Bytes())); err != nil {
        t.Errorf("Decode after metadata encode failed: %v", err)
    }
}

func TestEncodeMetadataForcesVP8X(t *testing.T) {
    img := generateTestImageNRGBA(8, 8, 1.0, false)

    var buf bytes.Buffer
    // Only ICC set, UseExtendedFormat intentionally false: VP8X must still
    // be emitted so the ICCP chunk is spec-legal.
    if err := Encode(&buf, img, &Options{ICCProfile: []byte("p")}); err != nil {
        t.Fatal(err)
    }
    chunks := parseWebPChunks(t, buf.Bytes())
    if chunks[0].id != "VP8X" {
        t.Fatalf("first chunk = %q, want VP8X", chunks[0].id)
    }
}

func TestEncodeNoMetadataNoVP8X(t *testing.T) {
    img := generateTestImageNRGBA(8, 8, 1.0, false)

    var buf bytes.Buffer
    if err := Encode(&buf, img, nil); err != nil {
        t.Fatal(err)
    }
    // Guards the byte-for-byte default: no metadata and no UseExtendedFormat
    // means no VP8X wrapper, just a bare VP8L chunk.
    if got := chunkIDs(parseWebPChunks(t, buf.Bytes())); !reflect.DeepEqual(got, []string{"VP8L"}) {
        t.Fatalf("chunk order = %v, want [VP8L]", got)
    }
}

func TestEncodeMetadataLossyOpaque(t *testing.T) {
    img := generateTestImageNRGBA(32, 32, 1.0, false) // opaque

    var buf bytes.Buffer
    if err := Encode(&buf, img, &Options{
        Lossy:      true,
        Quality:    75,
        Method:     0,
        ICCProfile: sampleICC,
        EXIF:       sampleEXIF,
        XMP:        sampleXMP,
    }); err != nil {
        t.Fatal(err)
    }

    chunks := parseWebPChunks(t, buf.Bytes())
    want := []string{"VP8X", "ICCP", "VP8 ", "EXIF", "XMP "}
    if got := chunkIDs(chunks); !reflect.DeepEqual(got, want) {
        t.Fatalf("chunk order = %v, want %v", got, want)
    }
    assertChunkPayload(t, chunks, "ICCP", sampleICC)
    assertChunkPayload(t, chunks, "XMP ", sampleXMP)

    if _, err := Decode(bytes.NewReader(buf.Bytes())); err != nil {
        t.Errorf("Decode after lossy metadata encode failed: %v", err)
    }
}

func TestEncodeMetadataLossyAlpha(t *testing.T) {
    img := generateTestImageNRGBA(32, 32, 1.0, true) // has alpha

    var buf bytes.Buffer
    if err := Encode(&buf, img, &Options{
        Lossy:      true,
        ICCProfile: sampleICC,
        EXIF:       sampleEXIF,
        XMP:        sampleXMP,
    }); err != nil {
        t.Fatal(err)
    }

    chunks := parseWebPChunks(t, buf.Bytes())
    want := []string{"VP8X", "ICCP", "ALPH", "VP8 ", "EXIF", "XMP "}
    if got := chunkIDs(chunks); !reflect.DeepEqual(got, want) {
        t.Fatalf("chunk order = %v, want %v", got, want)
    }
    if f := chunks[0].payload[0]; f&0x10 == 0 {
        t.Errorf("VP8X flags = %#x, want alpha bit (0x10) set", f)
    }

    if _, err := Decode(bytes.NewReader(buf.Bytes())); err != nil {
        t.Errorf("Decode after lossy+alpha metadata encode failed: %v", err)
    }
}

func TestEncodeMetadataAnimation(t *testing.T) {
    f1 := generateTestImageNRGBA(16, 16, 1.0, false)
    f2 := generateTestImageNRGBA(16, 16, 0.5, false)
    ani := &Animation{
        Images:    []image.Image{f1, f2},
        Durations: []uint{100, 100},
        Disposals: []uint{0, 0},
        LoopCount: 0,
    }

    var buf bytes.Buffer
    if err := EncodeAll(&buf, ani, &Options{
        ICCProfile: sampleICC,
        EXIF:       sampleEXIF,
        XMP:        sampleXMP,
    }); err != nil {
        t.Fatal(err)
    }

    ids := chunkIDs(parseWebPChunks(t, buf.Bytes()))
    if len(ids) < 5 {
        t.Fatalf("too few chunks: %v", ids)
    }
    if ids[0] != "VP8X" || ids[1] != "ICCP" || ids[2] != "ANIM" {
        t.Errorf("leading chunks = %v, want VP8X, ICCP, ANIM", ids[:3])
    }
    if ids[len(ids)-2] != "EXIF" || ids[len(ids)-1] != "XMP " {
        t.Errorf("trailing chunks = %v, want ..., EXIF, XMP ", ids[len(ids)-2:])
    }
    for _, id := range ids[3 : len(ids)-2] {
        if id != "ANMF" {
            t.Errorf("frame region chunk = %q, want ANMF", id)
        }
    }
}
