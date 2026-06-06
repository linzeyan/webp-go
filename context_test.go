package gowebp

import (
    "bytes"
    "context"
    "image"
    "sync/atomic"
    "testing"
)

// cancelAfterFirstPoll reports no error on its first Err() poll (so encoding
// gets past the entry guard) and context.Canceled on every poll thereafter. It
// exercises the per-frame / per-macroblock-row checks that run *after* the
// entry guard. Err() is atomic so it is safe under the concurrent frame
// encoding in EncodeAll.
type cancelAfterFirstPoll struct {
    context.Context
    polls *int64
}

func (c cancelAfterFirstPoll) Err() error {
    if atomic.AddInt64(c.polls, 1) <= 1 {
        return nil
    }
    return context.Canceled
}

func cancelledCtx() context.Context {
    ctx, cancel := context.WithCancel(context.Background())
    cancel()
    return ctx
}

func TestEncodeContextCancelled(t *testing.T) {
    img := generateTestImageNRGBA(16, 16, 1.0, false)

    for _, tc := range []struct {
        name string
        o    *Options
    }{
        {"lossless", nil},
        {"lossy", &Options{Lossy: true, Quality: 75}},
    } {
        t.Run(tc.name, func(t *testing.T) {
            var buf bytes.Buffer
            if err := EncodeContext(cancelledCtx(), &buf, img, tc.o); err != context.Canceled {
                t.Fatalf("EncodeContext err = %v, want context.Canceled", err)
            }
        })
    }
}

func TestEncodeAllContextCancelled(t *testing.T) {
    f := generateTestImageNRGBA(16, 16, 1.0, false)
    ani := &Animation{
        Images:    []image.Image{f, f},
        Durations: []uint{100, 100},
        Disposals: []uint{0, 0},
    }

    var buf bytes.Buffer
    if err := EncodeAllContext(cancelledCtx(), &buf, ani, nil); err != context.Canceled {
        t.Fatalf("EncodeAllContext err = %v, want context.Canceled", err)
    }
}

// TestEncodeContextLossyCancelMidStream proves the macroblock-row poll fires:
// the entry guard passes (poll 1) and the first row check trips (poll 2).
func TestEncodeContextLossyCancelMidStream(t *testing.T) {
    img := generateTestImageNRGBA(16, 16, 1.0, false)
    var polls int64
    ctx := cancelAfterFirstPoll{Context: context.Background(), polls: &polls}

    var buf bytes.Buffer
    if err := EncodeContext(ctx, &buf, img, &Options{Lossy: true, Quality: 75}); err != context.Canceled {
        t.Fatalf("EncodeContext err = %v, want context.Canceled", err)
    }
    if polls < 2 {
        t.Errorf("polls = %d, want >= 2 (entry guard then row check)", polls)
    }
}

// TestEncodeAllContextCancelMidStream proves the per-frame poll fires after the
// entry guard: the first poll passes, later frame polls cancel. Frames encode
// concurrently, so it only asserts the call returns context.Canceled (which
// frame trips it is non-deterministic), not a poll count.
func TestEncodeAllContextCancelMidStream(t *testing.T) {
    f := generateTestImageNRGBA(16, 16, 1.0, false)
    ani := &Animation{
        Images:    []image.Image{f, f, f, f, f},
        Durations: []uint{100, 100, 100, 100, 100},
        Disposals: []uint{0, 0, 0, 0, 0},
    }
    var polls int64
    ctx := cancelAfterFirstPoll{Context: context.Background(), polls: &polls}

    var buf bytes.Buffer
    if err := EncodeAllContext(ctx, &buf, ani, nil); err != context.Canceled {
        t.Fatalf("EncodeAllContext err = %v, want context.Canceled", err)
    }
}

// TestEncodeContextBackgroundMatchesEncode guards that routing Encode through
// EncodeContext with a live context is behavior-preserving (byte-for-byte).
func TestEncodeContextBackgroundMatchesEncode(t *testing.T) {
    img := generateTestImageNRGBA(24, 24, 1.0, true)

    for _, tc := range []struct {
        name string
        o    *Options
    }{
        {"lossless", nil},
        {"lossy", &Options{Lossy: true, Quality: 75, Method: 2}},
    } {
        t.Run(tc.name, func(t *testing.T) {
            var a, b bytes.Buffer
            if err := Encode(&a, img, tc.o); err != nil {
                t.Fatalf("Encode: %v", err)
            }
            if err := EncodeContext(context.Background(), &b, img, tc.o); err != nil {
                t.Fatalf("EncodeContext: %v", err)
            }
            if !bytes.Equal(a.Bytes(), b.Bytes()) {
                t.Errorf("EncodeContext output differs from Encode (%d vs %d bytes)", a.Len(), b.Len())
            }
        })
    }
}
