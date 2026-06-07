package vp8enc

import "testing"

// TestBoolEncoderHelpers exercises the WriteFlag / WriteInt / WriteOptionalInt
// helpers and the Bytes / Len / Finish accessors.
func TestBoolEncoderHelpers(t *testing.T) {
	e := NewBoolEncoder()

	e.WriteFlag(true)
	e.WriteFlag(false)
	e.WriteInt(-5, 4, UniformProb)
	e.WriteInt(7, 4, UniformProb)
	e.WriteOptionalInt(0, 4, UniformProb) // "not present" branch
	e.WriteOptionalInt(3, 4, UniformProb) // "present" branch

	_ = e.Bytes() // pre-flush snapshot (diagnostic accessor)

	out := e.Finish()
	if len(out) == 0 {
		t.Fatal("Finish produced no bytes")
	}
	if e.Len() != len(out) {
		t.Errorf("Len() = %d, want %d", e.Len(), len(out))
	}
}

// TestDequantizeBlock checks that DequantizeBlock scales each coefficient by the
// DC factor at position 0 and the AC factor elsewhere.
func TestDequantizeBlock(t *testing.T) {
	const dc, ac = uint16(8), uint16(10)
	q := make([]int16, 16)
	for i := range q {
		q[i] = int16(i - 8)
	}
	coef := make([]int16, 16)

	DequantizeBlock(q, coef, dc, ac)

	if coef[0] != q[0]*int16(dc) {
		t.Errorf("DC coef[0] = %d, want %d", coef[0], q[0]*int16(dc))
	}
	rc := Zigzag4x4[1] // an AC position
	if coef[rc] != q[rc]*int16(ac) {
		t.Errorf("AC coef[%d] = %d, want %d", rc, coef[rc], q[rc]*int16(ac))
	}
}

// TestWriteFilterHeaderOff exercises the loop-filter-off header path.
func TestWriteFilterHeaderOff(t *testing.T) {
	e := NewBoolEncoder()
	WriteFilterHeaderOff(e)
	if len(e.Finish()) == 0 {
		t.Error("Finish produced no bytes")
	}
}
