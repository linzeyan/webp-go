package main

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"io"
	"testing"

	"github.com/linzeyan/webp-go"
)

func TestInputName(t *testing.T) {
	if got := inputName("-"); got != "<stdin>" {
		t.Errorf("inputName(-) = %q, want <stdin>", got)
	}
	if got := inputName("a.png"); got != "a.png" {
		t.Errorf("inputName(a.png) = %q", got)
	}
}

// partialGIF builds a 2-frame animation: a full red frame with the given
// disposal, then a small green sub-rectangle.
func partialGIF(disposal byte) *gif.GIF {
	pal := color.Palette{color.RGBA{0, 0, 0, 0}, color.RGBA{255, 0, 0, 255}, color.RGBA{0, 255, 0, 255}}
	full := image.NewPaletted(image.Rect(0, 0, 8, 8), pal)
	for i := range full.Pix {
		full.Pix[i] = 1 // red
	}
	sub := image.NewPaletted(image.Rect(2, 2, 4, 4), pal)
	for i := range sub.Pix {
		sub.Pix[i] = 2 // green
	}
	return &gif.GIF{
		Image:     []*image.Paletted{full, sub},
		Delay:     []int{10, 10},
		Disposal:  []byte{disposal, gif.DisposalNone},
		LoopCount: 0,
		Config:    image.Config{Width: 8, Height: 8},
	}
}

func TestGifDisposalBackground(t *testing.T) {
	ani := gifToAnimation(partialGIF(gif.DisposalBackground))
	if len(ani.Images) != 2 {
		t.Fatalf("frames = %d, want 2", len(ani.Images))
	}
	// Frame 0 disposes to background, so frame 1 composites onto a cleared
	// canvas: a pixel outside the green sub-rect must be transparent, and the
	// sub-rect itself must be green.
	frame1 := ani.Images[1].(*image.RGBA)
	if a := frame1.RGBAAt(0, 0).A; a != 0 {
		t.Errorf("disposed pixel alpha = %d, want 0 (transparent)", a)
	}
	if c := frame1.RGBAAt(3, 3); c.G == 0 || c.R != 0 {
		t.Errorf("sub-rect pixel = %v, want green", c)
	}
}

func TestGifDisposalPrevious(t *testing.T) {
	ani := gifToAnimation(partialGIF(gif.DisposalPrevious))
	if len(ani.Images) != 2 {
		t.Fatalf("frames = %d, want 2", len(ani.Images))
	}
}

// TestGifConfigFallback covers the path where g.Config has no dimensions and
// the canvas size is taken from the first frame's bounds.
func TestGifConfigFallback(t *testing.T) {
	g := partialGIF(gif.DisposalNone)
	g.Config = image.Config{} // no width/height
	ani := gifToAnimation(g)
	if b := ani.Images[0].Bounds(); b != image.Rect(0, 0, 8, 8) {
		t.Errorf("fallback canvas = %v, want 8x8", b)
	}
}

func TestConvertDecodeError(t *testing.T) {
	if _, err := convert([]byte("not an image"), &gowebp.Options{}, 0); err == nil {
		t.Error("expected a decode error for garbage input")
	}
}

func TestRunBatchToStdout(t *testing.T) {
	if code := run([]string{"-o", "-", "a.png", "b.png"}, nil, io.Discard, io.Discard); code != 2 {
		t.Errorf("batch to stdout: exit %d, want 2", code)
	}
}

func TestRunMalformedStdin(t *testing.T) {
	if code := run([]string{"-"}, bytes.NewReader([]byte("garbage")), io.Discard, io.Discard); code != 1 {
		t.Errorf("malformed stdin: exit %d, want 1", code)
	}
}

func TestRunHelp(t *testing.T) {
	if code := run([]string{"-h"}, nil, io.Discard, io.Discard); code != 0 {
		t.Errorf("-h: exit %d, want 0", code)
	}
}

// TestRunFlags exercises the -near, -m (lossless effort), -lossless and -lossy
// option branches end-to-end on the stdin path.
func TestRunFlags(t *testing.T) {
	for _, args := range [][]string{
		{"-near", "2", "-"},
		{"-m", "1", "-"},
		{"-lossless", "-"},
		{"-lossy", "-q", "60", "-m", "2", "-"},
	} {
		var out bytes.Buffer
		if code := run(args, bytes.NewReader(pngBytes(t, gradient(8, 8))), &out, io.Discard); code != 0 {
			t.Errorf("args %v: exit %d, want 0", args, code)
			continue
		}
		if _, err := gowebp.Decode(bytes.NewReader(out.Bytes())); err != nil {
			t.Errorf("args %v: output is not valid WebP: %v", args, err)
		}
	}
}
