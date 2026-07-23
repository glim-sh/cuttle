package serve

import (
	"math"
	"testing"
)

// The notches MUST sum to the requested delta, or the page ends up scrolled to
// the wrong place.
func TestPlanScrollSumsToDelta(t *testing.T) {
	deltas := []struct{ dx, dy float64 }{{0, 600}, {0, -450}, {120, 800}, {0, 20}, {-300, 0}}
	for seed := range uint64(50) {
		for _, d := range deltas {
			evs := planScroll(newTestRNG(seed), d.dx, d.dy)
			var sx, sy float64
			for _, e := range evs {
				sx, sy = sx+e.dx, sy+e.dy
				if e.dt <= 0 {
					t.Fatalf("seed %d: non-positive dt", seed)
				}
			}
			if math.Abs(sx-d.dx) > 1e-6 || math.Abs(sy-d.dy) > 1e-6 {
				t.Fatalf("seed %d delta (%.0f,%.0f): notches sum to (%.6f,%.6f)", seed, d.dx, d.dy, sx, sy)
			}
		}
	}
}

func TestPlanScrollChunking(t *testing.T) {
	if big := planScroll(newTestRNG(1), 0, 1000); len(big) < 2 {
		t.Fatalf("a large scroll produced %d notches, expected several", len(big))
	}
	if small := planScroll(newTestRNG(1), 0, 10); len(small) != 1 {
		t.Fatalf("a sub-notch scroll produced %d notches, want 1", len(small))
	}
}

func TestScrollEnvelopeBounds(t *testing.T) {
	for i := range 101 {
		p := float64(i) / 100
		if v := scrollEnvelope(p); v < 0.4-1e-9 || v > 1+1e-9 {
			t.Fatalf("envelope(%.2f)=%.3f outside [0.4,1]", p, v)
		}
	}
}

func TestPlanScrollDeterministic(t *testing.T) {
	a := planScroll(newTestRNG(9), 40, 700)
	b := planScroll(newTestRNG(9), 40, 700)
	if len(a) != len(b) {
		t.Fatalf("nondeterministic length: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("notch %d differs: %+v vs %+v", i, a[i], b[i])
		}
	}
}
