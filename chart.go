package main

import (
	"strconv"
	"strings"
)

var sparkLevels = []rune("▁▂▃▄▅▆▇█")

// sparkline renders values as block glyphs scaled between the series min and
// max. If there are more values than width, they are sampled down to width
// points; fewer values render as-is.
func sparkline(values []float64, width int) string {
	if len(values) == 0 || width <= 0 {
		return ""
	}
	pts := resample(values, width)
	min, max := bounds(pts)
	span := max - min
	var b strings.Builder
	for _, v := range pts {
		level := 0
		if span > 0 {
			level = int((v - min) / span * float64(len(sparkLevels)-1))
		}
		b.WriteRune(sparkLevels[clamp(level, 0, len(sparkLevels)-1)])
	}
	return b.String()
}

// areaChart renders values as a filled column chart height rows tall, scaled
// between lo and hi. Row 0 of the returned slice is the top row, so the caller
// can join them with the y-axis labels in the same order.
func areaChart(values []float64, width, height int, lo, hi float64) []string {
	if width <= 0 || height <= 0 {
		return nil
	}
	rows := make([]string, height)
	if len(values) == 0 {
		for i := range rows {
			rows[i] = strings.Repeat(" ", width)
		}
		return rows
	}
	pts := resample(values, width)
	span := hi - lo
	if span <= 0 {
		span = 1
	}
	// Each row holds 8 sub-levels, so a value maps onto height*8 eighths and the
	// partially filled row gets the matching block glyph.
	for r := 0; r < height; r++ {
		var b strings.Builder
		rowFromBottom := height - 1 - r
		for _, v := range pts {
			eighths := int((v - lo) / span * float64(height*8))
			eighths = clamp(eighths, 0, height*8)
			switch fill := eighths - rowFromBottom*8; {
			case fill >= 8:
				b.WriteRune('█')
			case fill <= 0:
				b.WriteRune(' ')
			default:
				b.WriteRune(sparkLevels[fill-1])
			}
		}
		// Left-pad the resampled series so a short history hugs the right edge,
		// keeping "now" in the same column as the axis regardless of point count.
		rows[r] = strings.Repeat(" ", clamp(width-len(pts), 0, width)) + b.String()
	}
	return rows
}

// gauge renders a proportional bar, e.g. "████████░░░░░░░░". frac is clamped to
// 0..1 so an over-limit utilization still renders a full bar.
func gauge(frac float64, width int) string {
	if width <= 0 {
		return ""
	}
	filled := clamp(int(frac*float64(width)+0.5), 0, width)
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

// resample reduces or passes through values to at most width points by
// averaging each output bucket's source range.
func resample(values []float64, width int) []float64 {
	if len(values) <= width {
		return values
	}
	out := make([]float64, width)
	for i := 0; i < width; i++ {
		start := i * len(values) / width
		end := (i + 1) * len(values) / width
		if end <= start {
			end = start + 1
		}
		var sum float64
		for j := start; j < end; j++ {
			sum += values[j]
		}
		out[i] = sum / float64(end-start)
	}
	return out
}

func bounds(vs []float64) (min, max float64) {
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

// itoa is strconv.Itoa under a shorter name, for the many inline count labels.
func itoa(n int) string { return strconv.Itoa(n) }
