package main

import (
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
