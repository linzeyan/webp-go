package main

import (
	"bytes"
	"image/gif"
	"testing"

	"github.com/linzeyan/webp-go"
)

func TestCheckPixels(t *testing.T) {
	cases := []struct {
		w, h, frames, max int64
		wantErr           bool
	}{
		{100, 100, 1, 10000, false},      // exactly at budget
		{100, 100, 1, 9999, true},        // 1 over
		{1000, 1000, 1, 0, false},        // 0 == unlimited
		{1 << 20, 1 << 20, 1, 100, true}, // huge dims, must not overflow
		{10, 10, 100, 10000, false},      // frames exactly at budget
		{10, 10, 101, 10000, true},       // frames push 1 over
		{0, 0, 1, 100, false},            // invalid dims -> skipped
	}
	for _, c := range cases {
		if err := checkPixels(c.w, c.h, c.frames, c.max); (err != nil) != c.wantErr {
			t.Errorf("checkPixels(%d,%d,%d,%d) err=%v, wantErr=%v", c.w, c.h, c.frames, c.max, err, c.wantErr)
		}
	}
}

func TestConvertRejectsOversizedImage(t *testing.T) {
	img := pngBytes(t, gradient(32, 32)) // 1024 pixels
	if _, err := convert(img, &gowebp.Options{}, 100); err == nil {
		t.Error("expected rejection: 32x32 exceeds a 100-pixel budget")
	}
	if _, err := convert(img, &gowebp.Options{}, 0); err != nil {
		t.Errorf("unlimited budget should pass: %v", err)
	}
}

func TestConvertRejectsOversizedAnimation(t *testing.T) {
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, animGIF(3)); err != nil { // 8x8 x3 = 192 pixels
		t.Fatal(err)
	}
	if _, err := convert(buf.Bytes(), &gowebp.Options{}, 100); err == nil {
		t.Error("expected rejection: animation total exceeds a 100-pixel budget")
	}
	if _, err := convert(buf.Bytes(), &gowebp.Options{}, 0); err != nil {
		t.Errorf("unlimited budget should pass: %v", err)
	}
}

func TestReadInputLimit(t *testing.T) {
	old := maxInputBytes
	maxInputBytes = 8
	defer func() { maxInputBytes = old }()

	if _, err := readInput("-", bytes.NewReader([]byte("0123456789"))); err == nil {
		t.Error("expected error when stdin exceeds maxInputBytes")
	}
	if data, err := readInput("-", bytes.NewReader([]byte("hi"))); err != nil || string(data) != "hi" {
		t.Errorf("small input: data=%q err=%v", data, err)
	}
}
