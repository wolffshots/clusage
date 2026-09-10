package main

import (
	"sort"
	"strconv"
	"time"

	"github.com/NimbleMarkets/ntcharts/linechart/timeserieslinechart"
	"github.com/charmbracelet/lipgloss"
)

// gapCols is the floor on how wide a hole must be before the line breaks. A
// chart column is the smallest gap the eye can see, so a break at one column
// would cut the line on ordinary jitter between polls.
const gapCols = 2

// gapFactor multiplies a series own typical spacing to get its break point.
// Columns alone cannot answer this. Readings 15 minutes apart on a 6 hour span
// sit four columns apart, so a flat rule of two columns would cut the line at
// every single reading.
const gapFactor = 3

// gapLimit is how long a hole in stamps must be before it reads as a hole
// rather than as the normal wait between readings. It takes the median spacing
// of this series, times gapFactor, and never returns less than gapCols columns.
func gapLimit(stamps []time.Time, col time.Duration) time.Duration {
	limit := gapCols * col
	var gaps []time.Duration
	for i := 1; i < len(stamps); i++ {
		if d := stamps[i].Sub(stamps[i-1]); d > 0 {
			gaps = append(gaps, d)
		}
	}
	if len(gaps) == 0 {
		return limit
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i] < gaps[j] })
	if med := gapFactor * gaps[len(gaps)/2]; med > limit {
		limit = med
	}
	return limit
}

// chartLine is one series on a chart: the values, the time each was read, and
// which palette entry each value belongs in.
//
// band returns an index into the palette the chart was given. A series with one
// color returns the same index for every value. A series colored by threshold,
// such as utilization, returns the band the value falls in.
type chartLine struct {
	vals   []float64
	stamps []time.Time
	band   func(float64) int
}

// timeChart draws one or more series against a uniform time axis as a braille
// line chart.
//
// It answers three things the block area chart could not. A column is a fixed
// slice of [from, to), so the axis stays uniform however irregular the polls
// are. Points join with a line, so the shape between samples reads as a trend.
// A hole wider than gapCols columns breaks the line, so a stretch with no
// reading draws as a gap rather than as a long straight interpolation.
//
// The library styles a data set, not a point, so a run of samples that share a
// palette entry becomes one data set. A run starts with the last point of the
// run before it, so the two lines meet at the crossing instead of leaving a
// notch.
func timeChart(lines []chartLine, palette []lipgloss.Style, from, to time.Time,
	w, h int, lo, hi float64, yFmt func(float64) string,
	xFmt func(int, float64) string) string {
	if w <= 0 || h <= 0 || len(palette) == 0 {
		return ""
	}
	span := to.Sub(from)
	ts := timeserieslinechart.New(w, h,
		timeserieslinechart.WithAxesStyles(dimStyle, dimStyle),
		timeserieslinechart.WithYLabelFormatter(func(_ int, v float64) string {
			return yFmt(v)
		}),
		timeserieslinechart.WithXLabelFormatter(xFmt),
	)
	// The library labels the y-axis every yStep rows, from the bottom up, and
	// hides a repeat. Half the graph height gives the three labels the old
	// hand rolled chart drew: the floor, the middle, and the ceiling. An odd
	// graph height has no middle row, and the top label then lands a row short
	// of hi, so give up a row to make it even.
	if ts.GraphHeight()%2 == 1 && h > 1 {
		h--
		ts.Resize(w, h)
	}
	if gh := ts.GraphHeight(); gh >= 2 {
		ts.SetYStep(gh / 2)
	}

	col := time.Duration(int64(span) / int64(w))

	for li, line := range lines {
		maxGap := gapLimit(line.stamps, col)
		seg, segBand := -1, -1
		var prev time.Time
		var prevVal float64
		for i, v := range line.vals {
			if i >= len(line.stamps) {
				break
			}
			at := line.stamps[i]
			b := clamp(line.band(v), 0, len(palette)-1)
			// A new segment starts at the first point, after a hole, or at a
			// palette crossing. The crossing case carries the previous point
			// over, so the two colored lines touch.
			carry := false
			switch {
			case seg < 0:
				seg++
			case at.Sub(prev) > maxGap:
				seg++
			case b != segBand:
				seg++
				carry = true
			}
			// The name has to carry the series index too, or two series would
			// share a data set and draw one line through both.
			name := strconv.Itoa(li) + ":" + strconv.Itoa(seg)
			// Style every point, not only the ones that open a segment on a
			// palette change. A segment that opens after a hole keeps the band
			// it had before the hole, so a rule that fired on a change alone
			// left that data set on the library default and drew it uncolored.
			ts.SetDataSetStyle(name, palette[b])
			segBand = b
			if carry {
				ts.PushDataSet(name,
					timeserieslinechart.TimePoint{Time: prev, Value: prevVal})
			}
			ts.PushDataSet(name, timeserieslinechart.TimePoint{Time: at, Value: v})
			prev, prevVal = at, v
		}
	}

	// Set the ranges after every push. PushDataSet only ever grows an auto
	// range, so a series that never reaches hi would otherwise draw against a
	// shorter axis than the one the labels promise.
	ts.SetTimeRange(from, to)
	ts.SetYRange(lo, hi)
	ts.SetViewTimeAndYRange(from, to, lo, hi)
	ts.DrawBrailleAll()
	return ts.View()
}

// oneLine is the common case: a single series on its own chart.
func oneLine(vals []float64, stamps []time.Time, band func(float64) int) []chartLine {
	return []chartLine{{vals: vals, stamps: stamps, band: band}}
}

// solid is the band function for a series that carries no threshold to color
// against, so every value takes the first palette entry.
func solid(float64) int { return 0 }

// xTimeFormatter labels the time axis in local time, at the resolution the span
// can tell apart. The library hands the label a unix second count.
func xTimeFormatter(span time.Duration) func(int, float64) string {
	layout := "15:04"
	if span > 48*time.Hour {
		layout = "Mon 02"
	}
	return func(_ int, v float64) string {
		return time.Unix(int64(v), 0).Local().Format(layout)
	}
}
