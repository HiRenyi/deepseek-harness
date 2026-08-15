package cdp

import (
	"math"
	"testing"
)

func TestQuadCenter(t *testing.T) {
	// quad = [x1,y1, x2,y2, x3,y3, x4,y4] clockwise
	// square 10,10 -> 20,20: corners (10,10),(20,10),(20,20),(10,20)
	q := Quad{10, 10, 20, 10, 20, 20, 10, 20}
	x, y, ok := q.Center()
	if !ok {
		t.Fatal("expected ok=true for valid quad")
	}
	if x != 15 || y != 15 {
		t.Errorf("Center = (%v,%v), want (15,15)", x, y)
	}
}

func TestQuadCenterTooShort(t *testing.T) {
	q := Quad{1, 2, 3, 4}
	_, _, ok := q.Center()
	if ok {
		t.Error("expected ok=false for quad with <8 elements")
	}
}

func TestQuadCenterNil(t *testing.T) {
	var q Quad
	_, _, ok := q.Center()
	if ok {
		t.Error("expected ok=false for nil quad")
	}
}

func TestQuadCenterArbitrary(t *testing.T) {
	// asymmetric quad
	q := Quad{0, 0, 4, 0, 4, 2, 0, 2}
	x, y, ok := q.Center()
	if !ok {
		t.Fatal("expected ok=true")
	}
	if math.Abs(x-2) > 1e-9 || math.Abs(y-1) > 1e-9 {
		t.Errorf("Center=(%v,%v), want (2,1)", x, y)
	}
}
