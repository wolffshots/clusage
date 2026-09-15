package main

import (
	"math"
	"regexp"
	"strconv"
	"time"
)

// tauDivisor sets the smoothing horizon as a fraction of the window length. A
// larger value smooths more and reacts slower.
const tauDivisor = 8

// resetDrop is how far utilization must fall before it reads as a window
// rollover rather than as accounting noise. It is a 0..1 fraction, so 0.02 is
// two whole percent.
//
// A changed reset header looks like a better signal, and the design doc names
// it. It is not used, because a reset that rolls forward on every reading
// would make every pair a new segment, and the rate would never seed. A fall
// this large catches every real rollover.
const resetDrop = 0.02

// staleTaus is how many smoothing horizons may pass after the newest reading
// before burnRate refuses to report.
const staleTaus = 4

// minSpanDivisor sets the shortest span a rate may be measured over, as a
// fraction of the horizon. The utilization header carries two decimals, so one
// step is a whole percent. Measured over seconds that one step reads as
// hundreds of percent per hour. A span this long keeps the step small against
// the real movement.
const minSpanDivisor = 4

// defaultWindowLength is assumed for a window whose name carries no length,
// such as "overage".
const defaultWindowLength = 5 * time.Hour

// windowLengthRe matches the leading "<n><unit>" of a window name, so "7d-opus"
// reads as seven days.
var windowLengthRe = regexp.MustCompile(`^(\d+)([mhd])`)

// windowLength returns how long the window named by name covers. ok is false
// for a name that carries no length.
func windowLength(name string) (time.Duration, bool) {
	m := windowLengthRe.FindStringSubmatch(name)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return 0, false
	}
	switch m[2] {
	case "m":
		return time.Duration(n) * time.Minute, true
	case "h":
		return time.Duration(n) * time.Hour, true
	case "d":
		return time.Duration(n) * 24 * time.Hour, true
	}
	return 0, false
}

// tauFor returns the smoothing horizon for a window name.
func tauFor(name string) time.Duration {
	length, ok := windowLength(name)
	if !ok {
		length = defaultWindowLength
	}
	return length / tauDivisor
}

// ratePoint is one reading reduced to what a rate needs. The reset header is
// not carried, because resetDrop finds a rollover from the utilization alone.
type ratePoint struct {
	at   time.Time
	frac float64
}

// ratePoints pulls one window out of every reading, oldest first. A reading
// that carries no usable utilization for that window contributes no point.
func ratePoints(readings []Reading, name string) []ratePoint {
	var out []ratePoint
	for _, r := range readings {
		for _, w := range parseWindows(r.Headers) {
			if w.Name != name {
				continue
			}
			if f, ok := w.utilFrac(); ok {
				out = append(out, ratePoint{at: r.FetchedAt, frac: f})
			}
			break
		}
	}
	return out
}

// rateWalk measures a rate at every point over a trailing span. It returns the
// rate at every point, oldest first, and whether that point carries a usable
// rate. Both slices are the same length as pts, and index 0 is never usable
// because one point carries no rate.
//
// The span reaches back one horizon, and further when the readings are sparse,
// so a short gap between two readings cannot turn one quantization step into a
// huge rate. A per-pair average cannot do this: it takes the whole horizon to
// forget one bad pair, and it takes that bad pair whole when it seeds.
func rateWalk(pts []ratePoint, tau time.Duration) ([]float64, []bool) {
	vals := make([]float64, len(pts))
	ok := make([]bool, len(pts))
	minSpan := tau / minSpanDivisor
	start := 0 // the first point of the current segment
	for i := 1; i < len(pts); i++ {
		if pts[i].frac < pts[i-1].frac-resetDrop {
			// The window rolled over. Start a new segment rather than measure
			// across the fall.
			start = i
			continue
		}
		// Anchor at the oldest point within one horizon. Reach back further
		// while the span is too short to measure.
		//
		// ponytail: this rescans the span for every point, so the cost is the
		// point count times the points in one horizon. A history holds
		// hundreds of points, so the scan stays cheap. Carry a moving index if
		// that ever stops being true.
		a := i - 1
		for a > start && (pts[i].at.Sub(pts[a].at) < minSpan || pts[i].at.Sub(pts[a-1].at) <= tau) {
			a--
		}
		span := pts[i].at.Sub(pts[a].at)
		if span < minSpan {
			// The segment is too young to measure. Report nothing rather than
			// a number the readings cannot support.
			continue
		}
		// Utilization is a 0..1 fraction, so scale to whole percents.
		vals[i] = (pts[i].frac - pts[a].frac) * 100 / span.Hours()
		ok[i] = true
	}
	return vals, ok
}

// burnRate returns percent of the window consumed per hour, and ok=false when
// the history cannot support an estimate. That covers an empty or one-point
// history, a segment still too young to measure, and a newest reading too old
// to speak for now.
func burnRate(readings []Reading, name string, now time.Time) (float64, bool) {
	pts := ratePoints(readings, name)
	if len(pts) < 2 {
		return 0, false
	}
	tau := tauFor(name)
	gap := now.Sub(pts[len(pts)-1].at)
	if gap > staleTaus*tau {
		return 0, false
	}
	vals, ok := rateWalk(pts, tau)
	last := len(pts) - 1
	if !ok[last] {
		return 0, false
	}
	rate := vals[last]
	if gap > 0 {
		// Decay toward zero over the gap since the last reading, so a rate
		// measured an hour ago does not report as current.
		rate *= math.Exp(-gap.Seconds() / tau.Seconds())
	}
	return rate, true
}

// rateSeries returns the burn rate at every reading that carries one, oldest
// first, with the timestamp each rate belongs to. The history chart graphs
// this. It applies no staleness decay, because every point is dated.
func rateSeries(readings []Reading, name string) ([]float64, []time.Time) {
	pts := ratePoints(readings, name)
	vals, ok := rateWalk(pts, tauFor(name))
	var outVals []float64
	var outStamps []time.Time
	for i := range pts {
		if !ok[i] {
			continue
		}
		outVals = append(outVals, vals[i])
		outStamps = append(outStamps, pts[i].at)
	}
	return outVals, outStamps
}
