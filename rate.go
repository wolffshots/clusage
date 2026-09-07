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

// ratePoint is one reading reduced to what a rate needs. The reset header
// comes along so a caller can see where a window rolled over.
type ratePoint struct {
	at    time.Time
	frac  float64
	reset string
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
				out = append(out, ratePoint{at: r.FetchedAt, frac: f, reset: w.Reset})
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
