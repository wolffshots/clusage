package main

import (
	"strconv"
	"strings"
)

// gauge renders a proportional bar, e.g. "████████░░░░░░░░". frac is clamped to
// 0..1 so an over-limit utilization still renders a full bar.
func gauge(frac float64, width int) string {
	if width <= 0 {
		return ""
	}
	filled := clamp(int(frac*float64(width)+0.5), 0, width)
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

func bounds(vs []float64) (min, max float64) {
	if len(vs) == 0 {
		return 0, 0
	}
	min, max = vs[0], vs[0]
	for _, v := range vs {
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	return min, max
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// pct formats a 0..1 fraction as a whole-percent label, e.g. "61%".
func pct(frac float64) string {
	return strconv.Itoa(int(frac*100+0.5)) + "%"
}

// pctPerHour labels a burn rate axis, trimming the decimal on a large value so
// the label fits the 4 column gutter.
func pctPerHour(v float64) string {
	if v >= 10 {
		return strconv.FormatFloat(v, 'f', 0, 64)
	}
	return strconv.FormatFloat(v, 'f', 1, 64)
}

// itoa is strconv.Itoa under a shorter name, for the many inline count labels.
func itoa(n int) string { return strconv.Itoa(n) }
