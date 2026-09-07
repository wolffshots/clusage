package main

import (
	"testing"
	"time"
)

func TestWindowLength(t *testing.T) {
	cases := map[string]time.Duration{
		"5h":      5 * time.Hour,
		"7d":      7 * 24 * time.Hour,
		"7d-opus": 7 * 24 * time.Hour,
		"30m":     30 * time.Minute,
	}
	for name, want := range cases {
		got, ok := windowLength(name)
		if !ok || got != want {
			t.Fatalf("windowLength(%q) = %v, %v; want %v, true", name, got, ok, want)
		}
	}
	for _, name := range []string{"overage", "", "hd", "0h", "5y"} {
		if got, ok := windowLength(name); ok {
			t.Fatalf("windowLength(%q) = %v, true; want no parse", name, got)
		}
	}
}

func TestTauFor(t *testing.T) {
	if got, want := tauFor("5h"), 37*time.Minute+30*time.Second; got != want {
		t.Fatalf("tauFor(5h) = %v; want %v", got, want)
	}
	if got, want := tauFor("7d"), 21*time.Hour; got != want {
		t.Fatalf("tauFor(7d) = %v; want %v", got, want)
	}
	// A window that carries no length must still get a horizon.
	if tauFor("overage") != tauFor("5h") {
		t.Fatalf("tauFor(overage) = %v; want the 5h horizon %v", tauFor("overage"), tauFor("5h"))
	}
}

// points builds a series from a start fraction, a step in fraction per sample,
// and a fixed gap between samples.
func points(n int, start, step float64, gap time.Duration) []ratePoint {
	base := time.Date(2026, 9, 7, 9, 0, 0, 0, time.UTC)
	out := make([]ratePoint, n)
	for i := range out {
		out[i] = ratePoint{at: base.Add(time.Duration(i) * gap), frac: start + step*float64(i)}
	}
	return out
}

func TestRateWalkFlatSeriesGivesZero(t *testing.T) {
	vals, ok := rateWalk(points(6, 0.4, 0, 10*time.Minute), tauFor("5h"))
	if len(vals) != 6 || len(ok) != 6 {
		t.Fatalf("want 6 values and 6 flags, got %d and %d", len(vals), len(ok))
	}
	if ok[0] {
		t.Fatal("the first point cannot carry a rate")
	}
	if !ok[5] {
		t.Fatal("the last point must carry a rate")
	}
	if vals[5] != 0 {
		t.Fatalf("a flat series must give 0, got %v", vals[5])
	}
}

func TestRateWalkRecoversASteadySlope(t *testing.T) {
	// 0.02 of the window every 10 minutes is 2 percent per 10 minutes, so
	// 12 percent per hour. An average of a constant is that constant.
	vals, ok := rateWalk(points(8, 0.1, 0.02, 10*time.Minute), tauFor("5h"))
	if !ok[7] {
		t.Fatal("want a rate at the last point")
	}
	if got := vals[7]; got < 11.99 || got > 12.01 {
		t.Fatalf("want about 12%%/h, got %v", got)
	}
}

func TestRateWalkWeighsByElapsedTimeNotSampleCount(t *testing.T) {
	// A long steady climb, then one short fast pair. Time decay must let the
	// short pair move the estimate only a little, because it covers little
	// time. Per-sample weighting would let it dominate.
	pts := points(8, 0.1, 0.02, 10*time.Minute) // 12%/h
	last := pts[len(pts)-1]
	pts = append(pts, ratePoint{at: last.at.Add(30 * time.Second), frac: last.frac + 0.05})
	vals, ok := rateWalk(pts, tauFor("5h"))
	if !ok[len(pts)-1] {
		t.Fatal("want a rate at the last point")
	}
	// The short pair alone reads as 360%/h. The estimate must stay near 12.
	if got := vals[len(pts)-1]; got < 12 || got > 30 {
		t.Fatalf("want the estimate to stay near 12%%/h, got %v", got)
	}
}

func TestRateWalkShortInputs(t *testing.T) {
	vals, ok := rateWalk(nil, tauFor("5h"))
	if len(vals) != 0 || len(ok) != 0 {
		t.Fatal("an empty input must give empty output")
	}
	vals, ok = rateWalk(points(1, 0.4, 0, time.Minute), tauFor("5h"))
	if len(vals) != 1 || ok[0] {
		t.Fatalf("one point carries no rate, got %v %v", vals, ok)
	}
}

func TestRateWalkIgnoresADuplicateTimestamp(t *testing.T) {
	pts := points(3, 0.1, 0.02, 10*time.Minute)
	pts = append(pts, ratePoint{at: pts[2].at, frac: pts[2].frac + 0.3})
	vals, ok := rateWalk(pts, tauFor("5h"))
	// The duplicate carries no elapsed time, so it must not divide by zero and
	// must not change the estimate.
	if !ok[3] || vals[3] != vals[2] {
		t.Fatalf("a zero gap must carry the previous estimate, got %v %v", vals, ok)
	}
}

func TestRatePointsKeepsTheResetHeader(t *testing.T) {
	readings := []Reading{{
		FetchedAt: time.Date(2026, 9, 7, 9, 0, 0, 0, time.UTC),
		Headers: map[string]string{
			"anthropic-ratelimit-unified-5h-utilization": "0.61",
			"anthropic-ratelimit-unified-5h-reset":       "1787584800",
			"anthropic-ratelimit-unified-7d-utilization": "0.10",
		},
	}}
	got := ratePoints(readings, "5h")
	if len(got) != 1 || got[0].frac != 0.61 || got[0].reset != "1787584800" {
		t.Fatalf("ratePoints dropped a field: %+v", got)
	}
	// A reading with no utilization for that window contributes no point.
	if n := len(ratePoints(readings, "overage")); n != 0 {
		t.Fatalf("want no points for an absent window, got %d", n)
	}
}
