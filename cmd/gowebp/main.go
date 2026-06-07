// Command gowebp converts images to WebP from the shell.
//
// It reads PNG, JPEG, GIF or WebP and writes WebP, defaulting to lossless
// VP8L. Animated GIFs become animated WebP. Multiple inputs are converted in
// one run, and "-" streams via stdin/stdout.
//
//	go install github.com/linzeyan/webp-go/cmd/gowebp@latest
//	gowebp photo.png                  # -> photo.webp (lossless)
//	gowebp -lossy -q 80 photo.jpg     # -> photo.webp (lossy)
//	gowebp anim.gif                   # -> anim.webp (animated)
//	cat a.png | gowebp - > a.webp     # stdin -> stdout
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path/filepath"

	"github.com/linzeyan/webp-go"
)

// defaultMaxPixels is the default -max-pixels budget: 16384*16384, the
// encoder's own maximum single-image area. Inputs whose width*height (or, for
// animations, width*height*frames) exceed it are rejected as decompression
// bombs before they are decoded or composited.
const defaultMaxPixels = 1 << 28

// maxInputBytes caps how many bytes one input may have, bounding the memory a
// never-ending stdin pipe or an oversized file can consume. A var so tests can
// lower it.
var maxInputBytes int64 = 256 << 20 // 256 MiB

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// readInput reads one input — a file, or stdin when in == "-" — with a hard
// byte cap so a huge or unbounded stream cannot exhaust memory.
func readInput(in string, stdin io.Reader) ([]byte, error) {
	r := stdin
	if in != "-" {
		f, err := os.Open(in)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		r = f
	}
	data, err := io.ReadAll(io.LimitReader(r, maxInputBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxInputBytes {
		return nil, fmt.Errorf("input exceeds %d MiB", maxInputBytes>>20)
	}
	return data, nil
}

// checkPixels rejects an input whose decoded pixel volume (width*height*frames)
// exceeds maxPixels, guarding against decompression bombs. maxPixels <= 0
// disables the check. The comparison never overflows int64.
func checkPixels(w, h, frames, maxPixels int64) error {
	if maxPixels <= 0 || w < 1 || h < 1 || frames < 1 {
		return nil
	}
	// "w > maxPixels/h" is "w*h > maxPixels" without overflowing. Once that
	// passes, w*h <= maxPixels, so multiplying the frame check is safe too.
	if w > maxPixels/h || w*h > maxPixels/frames {
		return fmt.Errorf("input too large: %d x %d x %d frame(s) exceeds -max-pixels=%d", w, h, frames, maxPixels)
	}
	return nil
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gowebp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		output   = fs.String("o", "", "output file, output directory (multiple inputs), or - for stdout")
		lossy    = fs.Bool("lossy", false, "encode with lossy VP8 (default: lossless VP8L)")
		lossless = fs.Bool("lossless", false, "force lossless VP8L (the default)")
		quality  = fs.Float64("q", 75, "lossy quality, 0-100")
		method   = fs.Int("m", -1, "method/effort 0-6 (default 4 lossy, 0 lossless)")
		near     = fs.Int("near", 0, "near-lossless: max low R/G/B bits dropped, 0-5 (lossless)")
		maxPix   = fs.Int64("max-pixels", defaultMaxPixels, "reject inputs over this many pixels (width*height*frames); 0 = unlimited")
	)
	fs.Usage = func() {
		fmt.Fprint(stderr, "gowebp converts images (PNG/JPEG/GIF/WebP) to WebP.\n\n"+
			"Usage:\n  gowebp [flags] <input>...\n\n"+
			"Use - for stdin/stdout. Animated GIFs become animated WebP.\n"+
			"Default mode is lossless.\n\nFlags:\n")
		fs.PrintDefaults()
	}

	// Parse flags allowing them before or after inputs: the stdlib flag
	// package stops at the first non-flag, so consume flags then one
	// positional per pass until everything is read.
	var inputs []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0
			}
			return 2
		}
		rest = fs.Args()
		if len(rest) == 0 {
			break
		}
		inputs = append(inputs, rest[0])
		rest = rest[1:]
	}

	if len(inputs) == 0 {
		fmt.Fprintln(stderr, "gowebp: no input files (use - for stdin)")
		fs.Usage()
		return 2
	}
	if *lossy && *lossless {
		fmt.Fprintln(stderr, "gowebp: -lossy and -lossless are mutually exclusive")
		return 2
	}

	o := &gowebp.Options{Lossy: *lossy && !*lossless}
	if o.Lossy {
		o.Quality = float32(*quality)
		o.Method = 4
	} else {
		o.NearLossless = *near
	}
	if *method >= 0 {
		o.Method = *method
	}

	batch := len(inputs) > 1
	if batch && *output == "-" {
		fmt.Fprintln(stderr, "gowebp: cannot write multiple inputs to stdout")
		return 2
	}

	failed := 0
	for _, in := range inputs {
		if err := convertOne(in, *output, batch, o, *maxPix, stdin, stdout); err != nil {
			fmt.Fprintf(stderr, "gowebp: %s: %v\n", inputName(in), err)
			failed++
		}
	}
	if failed > 0 {
		return 1
	}
	return 0
}

func inputName(in string) string {
	if in == "-" {
		return "<stdin>"
	}
	return in
}

// convertOne reads one input, encodes it to WebP, and writes it to the
// resolved destination.
func convertOne(in, outFlag string, batch bool, o *gowebp.Options, maxPixels int64, stdin io.Reader, stdout io.Writer) error {
	data, err := readInput(in, stdin)
	if err != nil {
		return err
	}

	webp, err := convert(data, o, maxPixels)
	if err != nil {
		return err
	}

	dst, err := resolveOutput(in, outFlag, batch)
	if err != nil {
		return err
	}
	if dst == "-" {
		_, err = stdout.Write(webp)
		return err
	}
	if dir := filepath.Dir(dst); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(dst, webp, 0o644)
}

// convert decodes the input image bytes and re-encodes them as WebP. A
// multi-frame GIF is converted to an animated WebP; everything else (including
// single-frame GIFs) becomes a still WebP.
func convert(data []byte, o *gowebp.Options, maxPixels int64) ([]byte, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	// Reject oversized inputs before decoding allocates the pixel buffer.
	if err := checkPixels(int64(cfg.Width), int64(cfg.Height), 1, maxPixels); err != nil {
		return nil, err
	}

	var buf bytes.Buffer

	if format == "gif" {
		g, err := gif.DecodeAll(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("decode gif: %w", err)
		}
		if len(g.Image) > 1 {
			// Each frame is composited onto a full-canvas RGBA, so bound the
			// total pixel volume before the per-frame cloning in gifToAnimation.
			if err := checkPixels(int64(cfg.Width), int64(cfg.Height), int64(len(g.Image)), maxPixels); err != nil {
				return nil, err
			}
			if err := gowebp.EncodeAll(&buf, gifToAnimation(g), o); err != nil {
				return nil, err
			}
			return buf.Bytes(), nil
		}
	}

	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if err := gowebp.Encode(&buf, img, o); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// gifToAnimation composites GIF frames (which are often partial sub-rectangles
// with their own disposal methods) into full-canvas frames and maps them onto
// a gowebp.Animation.
//
// Every WebP frame is the complete canvas at offset (0,0) with disposal set to
// dispose-to-background over a transparent background. gowebp's ANMF uses alpha
// blending, so clearing to a transparent canvas and blending the full frame
// reproduces each composited frame exactly.
func gifToAnimation(g *gif.GIF) *gowebp.Animation {
	w, h := g.Config.Width, g.Config.Height
	if w == 0 || h == 0 {
		b := g.Image[0].Bounds()
		w, h = b.Dx(), b.Dy()
	}

	canvas := image.NewRGBA(image.Rect(0, 0, w, h))
	ani := &gowebp.Animation{
		LoopCount:       uint16(g.LoopCount),
		BackgroundColor: 0x00000000, // transparent
	}

	for i, frame := range g.Image {
		disposal := byte(0)
		if i < len(g.Disposal) {
			disposal = g.Disposal[i]
		}

		var saved *image.RGBA
		if disposal == gif.DisposalPrevious {
			saved = cloneRGBA(canvas)
		}

		// draw.Over honors the frame's transparent palette entry, leaving the
		// canvas untouched where the frame is transparent.
		draw.Draw(canvas, frame.Bounds(), frame, frame.Bounds().Min, draw.Over)

		ani.Images = append(ani.Images, cloneRGBA(canvas))

		delay := 10 // GIF delay is in 1/100 s; 0 (often "as fast as possible") → 100ms
		if i < len(g.Delay) && g.Delay[i] > 0 {
			delay = g.Delay[i]
		}
		ani.Durations = append(ani.Durations, uint(delay*10))
		ani.Disposals = append(ani.Disposals, 1)

		// Restore the canvas for the next frame per this frame's disposal.
		switch disposal {
		case gif.DisposalBackground:
			clearRect(canvas, frame.Bounds())
		case gif.DisposalPrevious:
			copy(canvas.Pix, saved.Pix)
		}
	}

	return ani
}

func cloneRGBA(src *image.RGBA) *image.RGBA {
	dst := image.NewRGBA(src.Bounds())
	copy(dst.Pix, src.Pix)
	return dst
}

func clearRect(img *image.RGBA, r image.Rectangle) {
	r = r.Intersect(img.Bounds())
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			img.SetRGBA(x, y, color.RGBA{})
		}
	}
}

// resolveOutput derives the destination path for an input. See the flag help
// for the -o semantics; "-" means stdout.
func resolveOutput(in, outFlag string, batch bool) (string, error) {
	if outFlag == "-" {
		return "-", nil
	}
	if in == "-" && outFlag == "" {
		return "-", nil
	}

	base := "stdin"
	if in != "-" {
		base = filepath.Base(in)
	}

	if outFlag == "" {
		return replaceExt(in, ".webp"), nil
	}

	// With multiple inputs, or when -o names an existing directory, -o is a
	// directory and each input lands inside it.
	if batch || isDir(outFlag) {
		return filepath.Join(outFlag, replaceExt(base, ".webp")), nil
	}
	return outFlag, nil
}

func replaceExt(p, ext string) string {
	return p[:len(p)-len(filepath.Ext(p))] + ext
}

func isDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}
