package serve

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"
)

func newTestRNG(seed uint64) *rand.Rand {
	return rand.New(rand.NewPCG(seed, seed^0x9e3779b9))
}

// TestInjectedIDBasesFitInt32 pins the invariant a live Chrome enforces but the
// fake CDP in these tests does not: it parses a CDP message id as a 32-bit int and
// rejects anything >= 2^31 with "Message must have integer 'id' property". Both
// injected-command ranges must therefore stay under MaxInt32, and must not overlap
// each other. humanizeIDBase was originally 3e9 - over the limit - so every
// injected mouse move was silently refused by Chrome; only a live drive caught it.
func TestInjectedIDBasesFitInt32(t *testing.T) {
	if humanizeIDBase >= math.MaxInt32 {
		t.Fatalf("humanizeIDBase %d >= MaxInt32 - Chrome will reject injected commands", humanizeIDBase)
	}
	if injectedIDBase >= math.MaxInt32 {
		t.Fatalf("injectedIDBase %d >= MaxInt32 - Chrome will reject injected commands", injectedIDBase)
	}
	if humanizeIDBase >= injectedIDBase {
		t.Fatalf("humanizeIDBase %d must stay below injectedIDBase %d so the ranges never overlap",
			humanizeIDBase, injectedIDBase)
	}
}

func totalDur(evs []mouseEvent) time.Duration {
	var t time.Duration
	for _, e := range evs {
		t += e.dt
	}
	return t
}

func TestPlanMouseMoveLandsExactlyOnTarget(t *testing.T) {
	for seed := range uint64(50) {
		evs := planMouseMove(newTestRNG(seed), 10, 10, 640, 480)
		last := evs[len(evs)-1]
		if last.x != 640 || last.y != 480 {
			t.Fatalf("seed %d: final sample (%.2f,%.2f), want (640,480)", seed, last.x, last.y)
		}
	}
}

func TestPlanMouseMovePositiveIntervalsAndBoundedSteps(t *testing.T) {
	evs := planMouseMove(newTestRNG(1), 0, 0, 500, 300)
	if len(evs) < moveMinSteps || len(evs) > moveMaxSteps+1 { // +1 for optional overshoot settle
		t.Fatalf("step count %d out of bounds [%d,%d]", len(evs), moveMinSteps, moveMaxSteps+1)
	}
	for i, e := range evs {
		if e.dt <= 0 {
			t.Fatalf("event %d has non-positive dt %v", i, e.dt)
		}
	}
}

func TestPlanMouseMoveZeroDistanceIsSingleSample(t *testing.T) {
	evs := planMouseMove(newTestRNG(7), 100, 100, 100, 100)
	if len(evs) != 1 {
		t.Fatalf("zero-distance move produced %d samples, want 1", len(evs))
	}
}

// Fitts' law: averaged over seeds, a longer move must take longer than a short
// one. Averaging washes out the per-move log-normal jitter.
func TestPlanMouseMoveDurationScalesWithDistance(t *testing.T) {
	const n = 200
	var short, long time.Duration
	for seed := range uint64(n) {
		short += totalDur(planMouseMove(newTestRNG(seed), 0, 0, 40, 0))
		long += totalDur(planMouseMove(newTestRNG(seed), 0, 0, 1200, 0))
	}
	if long <= short {
		t.Fatalf("Fitts violated: mean long move %v <= mean short move %v", long/n, short/n)
	}
	// And the long move should be materially slower, not marginally.
	if long < short*2 {
		t.Errorf("expected long move >= 2x short; short=%v long=%v", short/n, long/n)
	}
}

// Intervals must be right-skewed (log-normal), not symmetric: the mean sits above
// the median for a positively skewed sample.
func TestPlanMouseMoveIntervalsAreRightSkewed(t *testing.T) {
	var dts []float64
	for seed := range uint64(100) {
		for _, e := range planMouseMove(newTestRNG(seed), 0, 0, 800, 600) {
			dts = append(dts, float64(e.dt.Microseconds()))
		}
	}
	mean, median := meanF(dts), medianF(dts)
	if mean <= median {
		t.Fatalf("intervals not right-skewed: mean %.1f <= median %.1f", mean, median)
	}
}

func TestPlanMouseMoveIsDeterministicPerSeed(t *testing.T) {
	a := planMouseMove(newTestRNG(42), 5, 5, 300, 200)
	b := planMouseMove(newTestRNG(42), 5, 5, 300, 200)
	if len(a) != len(b) {
		t.Fatalf("nondeterministic length: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("event %d differs: %+v vs %+v", i, a[i], b[i])
		}
	}
}

// The trajectory must stay near the straight line - a human arcs, but does not
// wander wildly. Bound the max perpendicular deviation to a fraction of distance.
func TestPlanMouseMoveStaysNearPath(t *testing.T) {
	fromX, fromY, toX, toY := 0.0, 0.0, 1000.0, 0.0
	dist := 1000.0
	for seed := range uint64(50) {
		for _, e := range planMouseMove(newTestRNG(seed), fromX, fromY, toX, toY) {
			// perpendicular distance from the x-axis is just |y| here
			if math.Abs(e.y) > 0.4*dist {
				t.Fatalf("seed %d: sample deviates %.1f px (> 40%% of dist)", seed, e.y)
			}
		}
	}
}

func meanF(xs []float64) float64 {
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func medianF(xs []float64) float64 {
	c := append([]float64(nil), xs...)
	// simple insertion of a sort via stdlib would be cleaner; keep the test dep-free
	for i := 1; i < len(c); i++ {
		for j := i; j > 0 && c[j-1] > c[j]; j-- {
			c[j-1], c[j] = c[j], c[j-1]
		}
	}
	n := len(c)
	if n%2 == 1 {
		return c[n/2]
	}
	return (c[n/2-1] + c[n/2]) / 2
}
