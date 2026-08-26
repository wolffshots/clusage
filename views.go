package main

import (
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// Adaptive colours so the UI stays readable on dark and light terminals.
var (
	accent    = lipgloss.AdaptiveColor{Light: "#6C3FC4", Dark: "#B58BFF"}
	positive  = lipgloss.AdaptiveColor{Light: "#1F7A3D", Dark: "#4ADE80"}
	negative  = lipgloss.AdaptiveColor{Light: "#B01919", Dark: "#F87171"}
	dim       = lipgloss.AdaptiveColor{Light: "#6B7280", Dark: "#7A828E"}
	fg        = lipgloss.AdaptiveColor{Light: "#1A1A1A", Dark: "#E5E7EB"}
	warnColor = lipgloss.AdaptiveColor{Light: "#9A6700", Dark: "#FBBF24"}
)

var (
	tabActiveStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FFFFFF")).
			Background(accent).
			Bold(true).
			Padding(0, 2)

	tabInactiveStyle = lipgloss.NewStyle().Foreground(dim).Padding(0, 2)
	tabBarStyle      = lipgloss.NewStyle().Padding(0, 0, 1, 0)

	titleStyle    = lipgloss.NewStyle().Foreground(accent).Bold(true)
	positiveStyle = lipgloss.NewStyle().Foreground(positive)
	negativeStyle = lipgloss.NewStyle().Foreground(negative)
	dimStyle      = lipgloss.NewStyle().Foreground(dim)
	warnStyle     = lipgloss.NewStyle().Foreground(warnColor)
	labelStyle    = lipgloss.NewStyle().Foreground(dim)
	valueStyle    = lipgloss.NewStyle().Foreground(fg).Bold(true)
	errorStyle    = lipgloss.NewStyle().Foreground(negative).Bold(true)

	footerStyle = lipgloss.NewStyle().
			Foreground(dim).
			BorderTop(true).
			BorderStyle(lipgloss.NormalBorder()).
			BorderForeground(dim)
)

// loadBands are the utilization colour bands, lightest load first.
var loadBands = [3]lipgloss.Style{positiveStyle, warnStyle, negativeStyle}

// loadBand is the band a utilization fraction falls in: 0 under 60%, 1 under
// 85%, 2 above. Chart code groups columns by band, which needs a comparable
// value rather than a style.
func loadBand(frac float64) int {
	switch {
	case frac >= 0.85:
		return 2
	case frac >= 0.60:
		return 1
	default:
		return 0
	}
}

// loadStyle colours a utilization fraction green under 60%, amber under 85%,
// red above. The same thresholds drive the gauges and the history lines so a
// colour means one thing across the whole UI.
func loadStyle(frac float64) lipgloss.Style { return loadBands[loadBand(frac)] }

// colorCols paints a chart row one column at a time, so every bar carries the
// colour of the value it shows. Colouring the whole chart by the newest reading
// hid the history: a run that climbed from 5% to 95% came out all red.
// Neighbouring columns in the same band share one escape sequence.
func colorCols(row string, cols []float64) string {
	runes := []rune(row)
	band := func(i int) int {
		if i >= len(cols) {
			return 0
		}
		return loadBand(cols[i])
	}
	var b strings.Builder
	for i := 0; i < len(runes); {
		j := i + 1
		for j < len(runes) && band(j) == band(i) {
			j++
		}
		b.WriteString(loadBands[band(i)].Render(string(runes[i:j])))
		i = j
	}
	return b.String()
}

// contentWidth is the usable width for a chart, leaving room for the y-axis
// labels and a little breathing space.
func contentWidth(total, reserve int) int {
	w := total - reserve
	if w < 10 {
		w = 10
	}
	if w > 160 {
		w = 160
	}
	return w
}

// ---- Now tab ---------------------------------------------------------------

// nowView renders one gauge per rate limit window, plus the reset time and the
// reading's age.
func (m model) nowView(height int) string {
	wins := currentWindows(m)
	if len(wins) == 0 {
		return dimStyle.Render("no anthropic-ratelimit-* windows in the last reading")
	}
	now := time.Now()
	barW := contentWidth(m.width, 34)

	var b strings.Builder
	for i, w := range wins {
		marker := "  "
		if i == m.selected {
			marker = titleStyle.Render("▸ ")
		}
		name := valueStyle.Render(padRight(w.Name, 10))

		frac, ok := w.utilFrac()
		if !ok {
			b.WriteString(marker + name + dimStyle.Render("no utilization header") + "\n")
			continue
		}
		style := loadStyle(frac)
		bar := style.Render(gauge(frac, barW))
		b.WriteString(marker + name + bar + " " + style.Render(padLeft(pct(frac), 4)) + "\n")

		var meta []string
		if w.Status != "" {
			meta = append(meta, statusDot(w.Status)+" "+w.Status)
		}
		if t, ok := w.resetTime(); ok {
			meta = append(meta, "resets "+t.Local().Format("Mon 15:04")+" ("+untilLabel(t, now)+")")
		}
		if len(meta) > 0 {
			b.WriteString("    " + dimStyle.Render(strings.Join(meta, "   ")) + "\n")
		}
		b.WriteString("\n")
	}

	age := now.Sub(m.latest.FetchedAt).Round(time.Second)
	b.WriteString(dimStyle.Render("read " + age.String() + " ago  ·  " + m.latest.Model))
	return clip(b.String(), height)
}

// statusDot colours a ● by the window's status: allowed (green), anything
// rejecting or throttling (red), otherwise amber.
func statusDot(status string) string {
	switch {
	case strings.Contains(status, "allowed_warning"):
		return warnStyle.Render("●")
	case strings.HasPrefix(status, "allowed"):
		return positiveStyle.Render("●")
	case status == "":
		return dimStyle.Render("●")
	default:
		return negativeStyle.Render("●")
	}
}

// untilLabel renders how long until t, or "passed" once it is behind now.
func untilLabel(t, now time.Time) string {
	left := t.Sub(now).Round(time.Minute)
	if left < 0 {
		return "passed"
	}
	return "in " + shortDur(left)
}

// ---- History tab -----------------------------------------------------------

// historyView graphs the selected window's utilization over the chosen span,
// with a sparkline row per other window underneath for comparison.
func (m model) historyView(height int) string {
	wins := currentWindows(m)
	if len(wins) == 0 {
		return dimStyle.Render("no windows to graph")
	}
	sel := wins[clamp(m.selected, 0, len(wins)-1)]
	span := historySpans[m.spanIdx]

	series, stamps := utilSeries(m.history, sel.Name)
	if len(series) == 0 {
		return titleStyle.Render(sel.Name+" utilization") + "  " + dimStyle.Render(span.label) +
			"\n\n" + dimStyle.Render("no readings in this span — press r to fetch, or s for a longer span")
	}

	chartW := contentWidth(m.width, 10)
	// Leave rows for the title, the x-axis line, the summary, and the other
	// windows' sparklines, so the chart never pushes the footer off screen.
	chartH := height - 5 - len(wins)
	chartH = clamp(chartH, 3, 16)

	style := loadStyle(series[len(series)-1])
	var b strings.Builder
	b.WriteString(titleStyle.Render(sel.Name+" utilization") +
		dimStyle.Render("   span ") + valueStyle.Render(span.label) +
		dimStyle.Render("   tab switches window, s switches span") + "\n\n")

	// Fixed 0..100% scale rather than min/max: the absolute distance to the
	// limit is the point of this graph, so an autoscaled 40-to-42% band would
	// read as a crisis.
	rows, cols := areaChart(series, chartW, chartH, 0, 1)
	for i, row := range rows {
		axis := ""
		switch i {
		case 0:
			axis = "100%"
		case len(rows) - 1:
			axis = "  0%"
		case len(rows) / 2:
			axis = " 50%"
		}
		b.WriteString(dimStyle.Render(padLeft(axis, 4)) + " " + colorCols(row, cols) + "\n")
	}
	b.WriteString(strings.Repeat(" ", 5) + dimStyle.Render(xAxis(stamps, chartW)) + "\n\n")

	min, max := bounds(series)
	b.WriteString(labelStyle.Render("  min ") + valueStyle.Render(pct(min)) +
		labelStyle.Render("  max ") + valueStyle.Render(pct(max)) +
		labelStyle.Render("  now ") + style.Render(pct(series[len(series)-1])) +
		labelStyle.Render("  n=") + valueStyle.Render(itoa(len(series))) + "\n\n")

	for _, w := range wins {
		s, _ := utilSeries(m.history, w.Name)
		line := dimStyle.Render("(no data)")
		if len(s) > 0 {
			glyphs, vals := sparkline(s, chartW)
			line = colorCols(glyphs, vals)
		}
		name := padRight(w.Name, 10)
		if w.Name == sel.Name {
			name = titleStyle.Render(name)
		} else {
			name = dimStyle.Render(name)
		}
		b.WriteString("  " + name + line + "\n")
	}
	return clip(b.String(), height)
}

// utilSeries pulls one window's utilization out of every reading, oldest first,
// skipping readings where that window is absent or unparseable.
func utilSeries(readings []Reading, name string) ([]float64, []time.Time) {
	var vals []float64
	var stamps []time.Time
	for _, r := range readings {
		for _, w := range parseWindows(r.Headers) {
			if w.Name != name {
				continue
			}
			if f, ok := w.utilFrac(); ok {
				vals = append(vals, f)
				stamps = append(stamps, r.FetchedAt)
			}
			break
		}
	}
	return vals, stamps
}

// xAxis labels the first and last sample times under the chart.
func xAxis(stamps []time.Time, width int) string {
	if len(stamps) == 0 || width < 12 {
		return ""
	}
	first := stamps[0].Local().Format("Mon 15:04")
	last := stamps[len(stamps)-1].Local().Format("Mon 15:04")
	gap := width - len(first) - len(last)
	if gap < 1 {
		return first
	}
	return first + strings.Repeat(" ", gap) + last
}

// ---- Tokens tab ------------------------------------------------------------

// tokensView graphs what clusage itself spent on probe calls: a cumulative
// total over the chosen span, a per-call sparkline, and the breakdown by
// stream with the cached share.
func (m model) tokensView(height int) string {
	span := historySpans[m.spanIdx]
	var b strings.Builder
	b.WriteString(titleStyle.Render("clusage token spend") +
		dimStyle.Render("   span ") + valueStyle.Render(span.label) +
		dimStyle.Render("   s switches span") + "\n\n")

	if len(m.tokens) == 0 {
		b.WriteString(dimStyle.Render("no calls in this span — press r to fetch, or s for a longer span") + "\n\n")
		b.WriteString(m.tokenTotalsBlock())
		return clip(b.String(), height)
	}

	cum, per, stamps := tokenSeries(m.tokens)
	chartW := contentWidth(m.width, 12)
	// Leave the 15 rows the title, the x-axis, the per-call line, the
	// breakdown, and the all-time line need, so the chart never pushes the
	// all-time total off the bottom.
	chartH := clamp(height-15, 3, 14)

	top := cum[len(cum)-1]
	// Spend has no limit to sit under, so the token chart stays one colour;
	// there is no band for a column to fall in.
	rows, _ := areaChart(cum, chartW, chartH, 0, top)
	for i, row := range rows {
		axis := ""
		switch i {
		case 0:
			axis = commas(int64(top + 0.5))
		case len(rows) - 1:
			axis = "0"
		}
		b.WriteString(dimStyle.Render(padLeft(axis, 6)) + " " + positiveStyle.Render(row) + "\n")
	}
	b.WriteString(strings.Repeat(" ", 7) + dimStyle.Render(xAxis(stamps, chartW)) + "\n\n")

	perGlyphs, _ := sparkline(per, chartW)
	b.WriteString("  " + labelStyle.Render(padRight("per call", 12)) +
		valueStyle.Render(perGlyphs) + "\n\n")

	var spanTotal tokenUse
	for _, s := range m.tokens {
		spanTotal = spanTotal.add(s.Used)
	}
	b.WriteString(tokenBreakdown(spanTotal, len(m.tokens), span.label))
	if spanTotal.CacheRead == 0 && spanTotal.CacheCreate == 0 {
		// A reader seeing two zero rows needs to know whether the counter is
		// broken or the call is simply too small to cache.
		b.WriteString(dimStyle.Render("  cache rows read 0 because a probe call is far under the ~1024 token cache minimum") + "\n")
	}
	b.WriteString("\n" + m.tokenTotalsBlock())
	return clip(b.String(), height)
}

// tokenBreakdown lists one row per token stream, plus the cached share and the
// per-call average.
func tokenBreakdown(u tokenUse, calls int, label string) string {
	var b strings.Builder
	row := func(name string, v int64) {
		b.WriteString("  " + labelStyle.Render(padRight(name, 12)) +
			valueStyle.Render(padLeft(commas(v), 10)) + "\n")
	}
	b.WriteString("  " + titleStyle.Render("last "+label) + "\n")
	row("input", u.Input)
	row("output", u.Output)
	row("cache write", u.CacheCreate)
	row("cache read", u.CacheRead)
	row("total", u.total())
	avg := int64(0)
	if calls > 0 {
		avg = u.total() / int64(calls)
	}
	b.WriteString("  " + labelStyle.Render(padRight("calls", 12)) +
		valueStyle.Render(padLeft(itoa(calls), 10)) +
		labelStyle.Render("   avg ") + valueStyle.Render(commas(avg)+"/call") +
		labelStyle.Render("   cached ") + valueStyle.Render(pct(u.cachedFrac())+" of input") + "\n")
	return b.String()
}

// tokenTotalsBlock reports the all-time spend, which is not limited by the span.
func (m model) tokenTotalsBlock() string {
	t := m.tokenTotal
	if m.tokenCalls == 0 {
		return dimStyle.Render("  all time     no calls recorded yet")
	}
	return "  " + labelStyle.Render(padRight("all time", 12)) +
		valueStyle.Render(commas(t.total())) + dimStyle.Render(" tokens over ") +
		valueStyle.Render(itoa(m.tokenCalls)) + dimStyle.Render(" calls  ·  ") +
		valueStyle.Render(commas(t.Input)) + dimStyle.Render(" in  ") +
		valueStyle.Render(commas(t.Output)) + dimStyle.Render(" out  ") +
		valueStyle.Render(commas(t.cached())) + dimStyle.Render(" cached")
}

// tokenSeries builds the cumulative total, the per-call total, and the call
// times, oldest first.
func tokenSeries(ss []TokenSample) (cum, per []float64, stamps []time.Time) {
	var running float64
	for _, s := range ss {
		v := float64(s.Used.total())
		running += v
		cum = append(cum, running)
		per = append(per, v)
		stamps = append(stamps, s.CalledAt)
	}
	return cum, per, stamps
}

// ---- Config tab ------------------------------------------------------------

// configView shows the effective config and what the schedule will do next.
// Editing happens in the file; the TUI only reports.
func (m model) configView() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Config") + "\n\n")

	row := func(label, value string) {
		b.WriteString("  " + labelStyle.Render(padRight(label, 18)) + valueStyle.Render(value) + "\n")
	}
	row("model", m.cfg.Model)
	row("threshold", itoa(m.cfg.ThresholdMinutes)+"m")
	row("history window", itoa(m.cfg.HistoryHours)+"h")

	b.WriteString("\n")
	if !cronValid(m.cfg.FetchCron) {
		label := m.cfg.FetchCron
		if strings.TrimSpace(label) == "" {
			label = "(unset)"
		}
		b.WriteString("  " + labelStyle.Render(padRight("fetch_cron", 18)) +
			errorStyle.Render(label) + dimStyle.Render("  needs 5 fields per expression, ; separates several") + "\n")
		b.WriteString("  " + labelStyle.Render(padRight("auto-fetch", 18)) + dimStyle.Render("disabled") + "\n")
	} else {
		row("fetch_cron", m.cfg.FetchCron)
		state := positiveStyle.Render("on")
		if !m.autoFetch {
			state = warnStyle.Render("paused (a resumes)")
		}
		b.WriteString("  " + labelStyle.Render(padRight("auto-fetch", 18)) + state + "\n")
		next := dimStyle.Render("never")
		if t, ok := nextFetch(m.cfg, time.Now()); ok {
			next = valueStyle.Render(t.Local().Format("Mon 15:04")) +
				dimStyle.Render("  ("+untilLabel(t, time.Now())+")")
		}
		b.WriteString("  " + labelStyle.Render(padRight("next fetch", 18)) + next + "\n")
	}

	b.WriteString("\n  " + labelStyle.Render(padRight("config file", 18)) + valueStyle.Render(m.cfgPath) + "\n")
	b.WriteString("\n" + dimStyle.Render("  Edit the file and restart to change these. Cron fields:") + "\n")
	b.WriteString(dimStyle.Render("  minute hour day-of-month month day-of-week, e.g. \"*/15 * * * *\" every 15 minutes,") + "\n")
	b.WriteString(dimStyle.Render("  \"0 9-17 * * 1-5\" hourly on weekday work hours, \"5 9 * * *;35 18 * * *\" twice a day.") + "\n")
	return b.String()
}

// ---- small helpers ---------------------------------------------------------

// commas groups a token count in threes, e.g. 1204 becomes "1,204".
func commas(n int64) string {
	s := strconv.FormatInt(n, 10)
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return sign + string(out)
}

func padRight(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return s + strings.Repeat(" ", w-len(s))
}

func padLeft(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return strings.Repeat(" ", w-len(s)) + s
}

// clip drops trailing lines that would not fit the body, so a long view cannot
// push the help footer off the screen.
func clip(s string, height int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if height > 0 && len(lines) > height {
		lines = lines[:height]
	}
	return strings.Join(lines, "\n")
}
