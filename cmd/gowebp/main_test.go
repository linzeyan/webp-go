package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/linzeyan/webp-go"
)

func gradient(w, h int) *image.NRGBA {
	m := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			m.SetNRGBA(x, y, color.NRGBA{uint8(x * 7), uint8(y * 7), 128, 255})
		}
	}
	return m
}

func pngBytes(t *testing.T, m image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, m); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func decodeNRGBA(t *testing.T, webp []byte) *image.NRGBA {
	t.Helper()
	dec, err := gowebp.Decode(bytes.NewReader(webp))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	out := image.NewNRGBA(dec.Bounds())
	draw.Draw(out, out.Bounds(), dec, dec.Bounds().Min, draw.Src)
	return out
}

// webpChunkIDs walks the RIFF container and returns the FourCCs in order.
func webpChunkIDs(t *testing.T, data []byte) []string {
	t.Helper()
	if len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		t.Fatal("not a RIFF/WEBP file")
	}
	var ids []string
	for i := 12; i+8 <= len(data); {
		id := string(data[i : i+4])
		size := int(binary.LittleEndian.Uint32(data[i+4 : i+8]))
		ids = append(ids, id)
		i += 8 + size
		if size&1 == 1 {
			i++
		}
	}
	return ids
}

func TestConvertLosslessRoundtrip(t *testing.T) {
	src := gradient(20, 16)
	out, err := convert(pngBytes(t, src), &gowebp.Options{})
	if err != nil {
		t.Fatal(err)
	}

	got := decodeNRGBA(t, out)
	if got.Bounds() != src.Bounds() {
		t.Fatalf("bounds %v vs %v", got.Bounds(), src.Bounds())
	}
	for y := 0; y < 16; y++ {
		for x := 0; x < 20; x++ {
			if got.NRGBAAt(x, y) != src.NRGBAAt(x, y) {
				t.Fatalf("pixel (%d,%d): %v vs %v", x, y, got.NRGBAAt(x, y), src.NRGBAAt(x, y))
			}
		}
	}
}

func TestConvertLossy(t *testing.T) {
	out, err := convert(pngBytes(t, gradient(32, 32)), &gowebp.Options{Lossy: true, Quality: 75, Method: 4})
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeNRGBA(t, out).Bounds(); got != image.Rect(0, 0, 32, 32) {
		t.Fatalf("bounds %v", got)
	}
}

func animGIF(frames int) *gif.GIF {
	pal := color.Palette{
		color.RGBA{0, 0, 0, 0}, color.RGBA{255, 0, 0, 255},
		color.RGBA{0, 255, 0, 255}, color.RGBA{0, 0, 255, 255},
	}
	g := &gif.GIF{LoopCount: 0, Config: image.Config{ColorModel: pal, Width: 8, Height: 8}}
	for i := 0; i < frames; i++ {
		p := image.NewPaletted(image.Rect(0, 0, 8, 8), pal)
		ci := uint8(i%3 + 1)
		for j := range p.Pix {
			p.Pix[j] = ci
		}
		g.Image = append(g.Image, p)
		g.Delay = append(g.Delay, 20) // 1/100 s
		g.Disposal = append(g.Disposal, gif.DisposalNone)
	}
	return g
}

func TestGifToAnimation(t *testing.T) {
	ani := gifToAnimation(animGIF(3))

	if len(ani.Images) != 3 || len(ani.Durations) != 3 || len(ani.Disposals) != 3 {
		t.Fatalf("slice lengths: %d/%d/%d", len(ani.Images), len(ani.Durations), len(ani.Disposals))
	}
	for i := range ani.Durations {
		if ani.Durations[i] != 200 { // 20 * 10 ms
			t.Errorf("duration[%d] = %d, want 200", i, ani.Durations[i])
		}
		if ani.Disposals[i] != 1 {
			t.Errorf("disposal[%d] = %d, want 1", i, ani.Disposals[i])
		}
		if b := ani.Images[i].Bounds(); b != image.Rect(0, 0, 8, 8) {
			t.Errorf("frame[%d] bounds %v, want full canvas", i, b)
		}
	}
	if ani.LoopCount != 0 {
		t.Errorf("loop = %d, want 0", ani.LoopCount)
	}
}

func TestConvertAnimatedGIF(t *testing.T) {
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, animGIF(3)); err != nil {
		t.Fatal(err)
	}

	out, err := convert(buf.Bytes(), &gowebp.Options{})
	if err != nil {
		t.Fatal(err)
	}

	ids := webpChunkIDs(t, out)
	if len(ids) < 2 || ids[0] != "VP8X" || ids[1] != "ANIM" {
		t.Fatalf("leading chunks = %v, want VP8X, ANIM", ids)
	}
	anmf := 0
	for _, id := range ids {
		if id == "ANMF" {
			anmf++
		}
	}
	if anmf != 3 {
		t.Fatalf("ANMF chunks = %d, want 3 (chunks: %v)", anmf, ids)
	}
}

func TestResolveOutput(t *testing.T) {
	cases := []struct {
		in, out string
		batch   bool
		want    string
	}{
		{"a.png", "", false, "a.webp"},
		{"dir/a.png", "", false, filepath.Join("dir", "a.webp")},
		{"a.png", "out.webp", false, "out.webp"},
		{"a.png", "-", false, "-"},
		{"-", "", false, "-"},
		{"-", "x.webp", false, "x.webp"},
		{"a.png", "dst", true, filepath.Join("dst", "a.webp")},
		{"sub/a.png", "dst", true, filepath.Join("dst", "a.webp")},
	}
	for _, c := range cases {
		got, err := resolveOutput(c.in, c.out, c.batch)
		if err != nil {
			t.Fatalf("%+v: %v", c, err)
		}
		if got != c.want {
			t.Errorf("resolveOutput(%q, %q, %v) = %q, want %q", c.in, c.out, c.batch, got, c.want)
		}
	}

	// Single input whose -o is an existing directory lands inside it.
	tmp := t.TempDir()
	if got, _ := resolveOutput("a.png", tmp, false); got != filepath.Join(tmp, "a.webp") {
		t.Errorf("existing-dir -o = %q, want %q", got, filepath.Join(tmp, "a.webp"))
	}
}

func TestRunSingleFile(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "x.png")
	if err := os.WriteFile(in, pngBytes(t, gradient(8, 8)), 0o644); err != nil {
		t.Fatal(err)
	}

	if code := run([]string{in}, nil, io.Discard, io.Discard); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}

	data, err := os.ReadFile(filepath.Join(dir, "x.webp"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gowebp.Decode(bytes.NewReader(data)); err != nil {
		t.Fatalf("output is not a valid WebP: %v", err)
	}
}

func TestRunStdinStdout(t *testing.T) {
	var out bytes.Buffer
	code := run([]string{"-"}, bytes.NewReader(pngBytes(t, gradient(8, 8))), &out, io.Discard)
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if _, err := gowebp.Decode(bytes.NewReader(out.Bytes())); err != nil {
		t.Fatalf("stdout is not a valid WebP: %v", err)
	}
}

func TestRunErrors(t *testing.T) {
	if code := run(nil, nil, io.Discard, io.Discard); code != 2 {
		t.Errorf("no inputs: exit %d, want 2", code)
	}
	if code := run([]string{"/no/such/file.png"}, nil, io.Discard, io.Discard); code != 1 {
		t.Errorf("missing file: exit %d, want 1", code)
	}
	if code := run([]string{"-lossy", "-lossless", "x.png"}, nil, io.Discard, io.Discard); code != 2 {
		t.Errorf("mutually exclusive: exit %d, want 2", code)
	}
}
