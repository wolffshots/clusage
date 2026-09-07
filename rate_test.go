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

// readingsFrom turns rate points into readings for the public entry points.
func readingsFrom(name string, pts []ratePoint) []Reading {
	out := make([]Reading, len(pts))
	for i, p := range pts {
		h := map[string]string{
			"anthropic-ratelimit-unified-" + name + "-utilization": ftoa(p.frac),
			"anthropic-ratelimit-unified-" + name + "-status":      "allowed",
		}
		if p.reset != "" {
			h["anthropic-ratelimit-unified-"+name+"-reset"] = p.reset
		}
		out[i] = Reading{FetchedAt: p.at, Model: "claude-opus-5", Headers: h}
	}
	return out
}

func TestRateWalkBreaksAtAReset(t *testing.T) {
	// Climb to 0.9, then the window resets to 0.02 and climbs again.
	pts := points(6, 0.6, 0.06, 10*time.Minute)
	base := pts[len(pts)-1].at
	for i := 0; i < 4; i++ {
		pts = append(pts, ratePoint{
			at:   base.Add(time.Duration(i+1) * 10 * time.Minute),
			frac: 0.02 + 0.03*float64(i),
		})
	}
	vals, ok := rateWalk(pts, tauFor("5h"))
	// The point right after the fall opens a new segment and carries no rate.
	if ok[6] {
		t.Fatalf("the point after a reset must carry no rate, got %v", vals[6])
	}
	// Two points into the new segment the rate is positive and reflects the
	// new climb of 3 percent per 10 minutes, so 18 percent per hour.
	if !ok[8] {
		t.Fatal("want a rate two points into the new segment")
	}
	if got := vals[8]; got < 17 || got > 19 {
		t.Fatalf("want about 18%%/h after the reset, got %v", got)
	}
}

func TestRateWalkAbsorbsASmallDip(t *testing.T) {
	// A 1 point wobble is accounting noise, not a reset. It must not restart
	// the average.
	pts := points(6, 0.3, 0.03, 10*time.Minute)
	pts[3].frac = pts[2].frac - 0.01
	_, ok := rateWalk(pts, tauFor("5h"))
	if !ok[3] {
		t.Fatal("a 1 point dip must not open a new segment")
	}
}

func TestBurnRateNeedsTwoPointsInASegment(t *testing.T) {
	now := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	if _, ok := burnRate(nil, "5h", now); ok {
		t.Fatal("an empty history must give ok=false")
	}
	one := readingsFrom("5h", []ratePoint{{at: now.Add(-time.Minute), frac: 0.4}})
	if _, ok := burnRate(one, "5h", now); ok {
		t.Fatal("one reading must give ok=false")
	}
}

func TestBurnRateDecaysAStaleHistory(t *testing.T) {
	pts := points(6, 0.1, 0.02, 10*time.Minute) // 12%/h
	readings := readingsFrom("5h", pts)
	last := pts[len(pts)-1].at

	fresh, ok := burnRate(readings, "5h", last.Add(time.Minute))
	if !ok || fresh < 11 || fresh > 13 {
		t.Fatalf("a fresh history must report about 12%%/h, got %v %v", fresh, ok)
	}
	// One horizon later the estimate must have decayed well below the fresh
	// value, because nothing has confirmed it since.
	aged, ok := burnRate(readings, "5h", last.Add(tauFor("5h")))
	if !ok {
		t.Fatal("one horizon of staleness must still report")
	}
	if aged >= fresh*0.5 {
		t.Fatalf("want decay, fresh=%v aged=%v", fresh, aged)
	}
	// Past the staleness cutoff it must refuse to report at all.
	if _, ok := burnRate(readings, "5h", last.Add(staleTaus*tauFor("5h")+time.Minute)); ok {
		t.Fatal("a history older than the cutoff must give ok=false")
	}
}

func TestRateSeriesSkipsUnusablePoints(t *testing.T) {
	pts := points(4, 0.1, 0.02, 10*time.Minute)
	vals, stamps := rateSeries(readingsFrom("5h", pts), "5h")
	if len(vals) != 3 || len(stamps) != 3 {
		t.Fatalf("want 3 usable rates from 4 points, got %d and %d", len(vals), len(stamps))
	}
	if !stamps[0].Equal(pts[1].at) {
		t.Fatalf("the first rate belongs to the second point, got %v", stamps[0])
	}
	if len(mustEmpty(rateSeries(nil, "5h"))) != 0 {
		t.Fatal("an empty history must give an empty series")
	}
}

func TestBurnRateForAWindowWithNoLength(t *testing.T) {
	// "overage" carries no length in its name, so tauFor falls back. It must
	// still produce a rate rather than refuse.
	pts := points(6, 0.1, 0.02, 10*time.Minute)
	readings := readingsFrom("overage", pts)
	now := pts[len(pts)-1].at.Add(time.Minute)
	got, ok := burnRate(readings, "overage", now)
	if !ok {
		t.Fatal("a window with no parseable length must still report a rate")
	}
	if got < 11 || got > 13 {
		t.Fatalf("want about 12%%/h, got %v", got)
	}
}

// mustEmpty lets the test above assert on the values slice alone.
func mustEmpty(vals []float64, _ []time.Time) []float64 { return vals }
