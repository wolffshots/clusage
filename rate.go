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

// rateWalk replays a time-decayed average over pts. It returns the rate at
// every point, oldest first, and whether that point carries a usable rate.
// Both slices are the same length as pts, and index 0 is never usable because
// one point carries no rate.
//
// The decay uses elapsed time rather than sample count, so a 30 second probe
// pair and a 15 minute cron pair carry the weight their spacing deserves.
func rateWalk(pts []ratePoint, tau time.Duration) ([]float64, []bool) {
	vals := make([]float64, len(pts))
	ok := make([]bool, len(pts))
	ewma, seeded := 0.0, false
	for i := 1; i < len(pts); i++ {
		if pts[i].frac < pts[i-1].frac-resetDrop {
			// The window rolled over. Start again rather than record the fall
			// as a large negative rate.
			ewma, seeded = 0, false
			vals[i], ok[i] = 0, false
			continue
		}
		dt := pts[i].at.Sub(pts[i-1].at)
		if dt > 0 {
			// Utilization is a 0..1 fraction, so scale to whole percents.
			inst := (pts[i].frac - pts[i-1].frac) * 100 / dt.Hours()
			if seeded {
				alpha := 1 - math.Exp(-dt.Seconds()/tau.Seconds())
				ewma += alpha * (inst - ewma)
			} else {
				ewma, seeded = inst, true
			}
		}
		vals[i], ok[i] = ewma, seeded
	}
	return vals, ok
}

// burnRate returns percent of the window consumed per hour, and ok=false when
// the history cannot support an estimate. That covers an empty or one-point
// history, the minutes right after a window rollover, and a newest reading too
// old to speak for now.
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
