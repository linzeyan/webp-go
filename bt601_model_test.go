package gowebp

import (
	"image"
	"image/color"
	"testing"
)

func near(t *testing.T, name string, got, want uint32) {
	t.Helper()
	d := int(got) - int(want)
	if d < 0 {
		d = -d
	}
	if d > 4 {
		t.Errorf("%s = %d, want ~%d (|Δ|=%d > 4)", name, got, want, d)
	}
}

// TestBT601ColorModels covers the RGB -> limited-range BT.601 color models and
// the alpha color type (the conversion path color.Model.Convert exercises).
func TestBT601ColorModels(t *testing.T) {
	src := color.RGBA{200, 100, 50, 255}
	yc, ok := BT601YCbCrColorModel.Convert(src).(BT601YCbCrColor)
	if !ok {
		t.Fatalf("Convert returned %T, want BT601YCbCrColor", BT601YCbCrColorModel.Convert(src))
	}
	r, g, b, a := yc.RGBA()
	if a != 0xffff {
		t.Errorf("alpha = %d, want 0xffff", a)
	}
	near(t, "R", r>>8, 200)
	near(t, "G", g>>8, 100)
	near(t, "B", b>>8, 50)

	// Converting an already-BT.601 color is the identity (no re-derivation).
	if got := BT601YCbCrColorModel.Convert(yc); got != color.Color(yc) {
		t.Errorf("Convert(BT601YCbCrColor) = %v, want identity", got)
	}

	// Extreme RGB exercises the clamp in both directions.
	_ = BT601YCbCrColorModel.Convert(color.RGBA{0, 0, 0, 255})
	_ = BT601YCbCrColorModel.Convert(color.RGBA{255, 255, 255, 255})

	// Alpha model + premultiplied RGBA.
	nyc, ok := BT601NYCbCrAColorModel.Convert(color.NRGBA{200, 100, 50, 128}).(BT601NYCbCrAColor)
	if !ok {
		t.Fatalf("Convert returned %T, want BT601NYCbCrAColor", BT601NYCbCrAColorModel.Convert(color.NRGBA{}))
	}
	if nyc.A != 128 {
		t.Errorf("alpha = %d, want 128", nyc.A)
	}
	pr, _, _, pa := nyc.RGBA()
	if pa != uint32(128)*0x101 {
		t.Errorf("premultiplied alpha = %d, want %d", pa, uint32(128)*0x101)
	}
	if pr > pa {
		t.Errorf("premultiplied R %d exceeds alpha %d", pr, pa)
	}
	if got := BT601NYCbCrAColorModel.Convert(nyc); got != color.Color(nyc) {
		t.Error("Convert(BT601NYCbCrAColor) should be the identity")
	}
}

// TestBT601Wrappers covers the BT601YCbCr / BT601NYCbCrA image wrappers,
// including their At, typed accessors, ColorModel, and out-of-bounds handling.
func TestBT601Wrappers(t *testing.T) {
	yimg := image.NewYCbCr(image.Rect(0, 0, 4, 4), image.YCbCrSubsampleRatio420)
	for i := range yimg.Y {
		yimg.Y[i] = 150
	}
	for i := range yimg.Cb {
		yimg.Cb[i], yimg.Cr[i] = 120, 130
	}
	wy := &BT601YCbCr{YCbCr: yimg}
	if wy.ColorModel() != BT601YCbCrColorModel {
		t.Error("BT601YCbCr.ColorModel mismatch")
	}
	if _, ok := wy.At(1, 1).(BT601YCbCrColor); !ok {
		t.Errorf("At returned %T, want BT601YCbCrColor", wy.At(1, 1))
	}
	if wy.BT601YCbCrAt(99, 99) != (BT601YCbCrColor{}) {
		t.Error("out-of-bounds BT601YCbCrAt should be the zero color")
	}

	aimg := image.NewNYCbCrA(image.Rect(0, 0, 4, 4), image.YCbCrSubsampleRatio420)
	for i := range aimg.Y {
		aimg.Y[i] = 150
	}
	for i := range aimg.Cb {
		aimg.Cb[i], aimg.Cr[i] = 120, 130
	}
	for i := range aimg.A {
		aimg.A[i] = 200
	}
	wa := &BT601NYCbCrA{NYCbCrA: aimg}
	if wa.ColorModel() != BT601NYCbCrAColorModel {
		t.Error("BT601NYCbCrA.ColorModel mismatch")
	}
	c, ok := wa.At(1, 1).(BT601NYCbCrAColor)
	if !ok {
		t.Fatalf("At returned %T, want BT601NYCbCrAColor", wa.At(1, 1))
	}
	if c.A != 200 {
		t.Errorf("alpha = %d, want 200", c.A)
	}
	if wa.BT601NYCbCrAAt(-1, -1) != (BT601NYCbCrAColor{}) {
		t.Error("out-of-bounds BT601NYCbCrAAt should be the zero color")
	}
}
