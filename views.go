package main

import (
	"os"
	"slices"
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

	// Hues for the overlay chart. They stay clear of green, amber and red, so no
	// line on that chart reads as a threshold verdict.
	seriesCyan = lipgloss.AdaptiveColor{Light: "#0E7490", Dark: "#22D3EE"}
	seriesPink = lipgloss.AdaptiveColor{Light: "#BE185D", Dark: "#F472B6"}
	seriesBlue = lipgloss.AdaptiveColor{Light: "#1D4ED8", Dark: "#60A5FA"}
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

// seriesPalette colours one chart line per window on the overlay chart. Every
// entry is a distinct hue, never a shade of the text colour: fg renders as
// near-white on a dark terminal, so a line painted with it reads as uncoloured
// however carefully the legend advertises it.
//
// The entries also avoid green, amber and red, because those three carry the
// threshold meaning everywhere else in the UI.
var seriesPalette = []lipgloss.Style{
	lipgloss.NewStyle().Foreground(accent),
	lipgloss.NewStyle().Foreground(seriesCyan),
	lipgloss.NewStyle().Foreground(seriesPink),
	lipgloss.NewStyle().Foreground(seriesBlue),
}

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

// sustainableRate is the burn rate that spends a window exactly over its own
// length, in percent per hour. A 5h window sustains 20 points per hour. A rate
// above this runs the window out early.
func sustainableRate(name string) float64 {
	length, ok := windowLength(name)
	if !ok {
		length = defaultWindowLength
	}
	return 100 / length.Hours()
}

// rateBand is the band a burn rate falls in: 0 at or under the sustainable
// rate, 1 under twice it, 2 above. It keys on the sustainable rate rather than
// on projected time to exhaustion, because a resampled chart column no longer
// pairs with a utilization value.
func rateBand(rate, sustainable float64) int {
	switch {
	case rate <= sustainable:
		return 0
	case rate <= 2*sustainable:
		return 1
	default:
		return 2
	}
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

// nowView renders one frame per rate limit window, each holding the gauge and
// the window's status, reset time and burn rate.
func (m model) nowView(height int) string {
	wins := currentWindows(m)
	if len(wins) == 0 {
		return dimStyle.Render("no anthropic-ratelimit-* windows in the last reading")
	}
	now := time.Now()
	frameW := contentWidth(m.width, frameCols) + frameCols
	// The gauge row is the bar, a space, and a 4 column percent label.
	barW := contentWidth(m.width, frameCols+5)

	var b strings.Builder
	for i, w := range wins {
		title := valueStyle.Render(w.Name)
		if i == m.selected {
			title = titleStyle.Render("▸ " + w.Name)
		}
		if w.Status != "" {
			title += "  " + statusDot(w.Status) + dimStyle.Render(" "+w.Status)
		}

		frac, ok := w.utilFrac()
		if !ok {
			b.WriteString(frame(title, dimStyle.Render("no utilization header"), frameW) + "\n")
			continue
		}
		style := loadStyle(frac)
		body := style.Render(gauge(frac, barW)) + " " + style.Render(padLeft(pct(frac), 4))

		var meta []string
		if t, ok := w.resetTime(); ok {
			meta = append(meta, "resets "+t.Local().Format("Mon 15:04")+" ("+untilLabel(t, now)+")")
		}
		rate, rateOK := burnRate(m.history, w.Name, now)
		meta = append(meta, burnLabel(rate, frac, rateOK))
		body += "\n" + dimStyle.Render(strings.Join(meta, "   "))
		b.WriteString(frame(title, body, frameW) + "\n")
	}

	age := now.Sub(m.latest.FetchedAt).Round(time.Second)
	b.WriteString(dimStyle.Render("  read " + age.String() + " ago  ·  " + m.latest.Model))
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

// burnLabel renders the burn rate and when the window fills at that rate. The
// projection targets 100 percent, because a usage viewer answers "when is this
// spent". The guard rail keeps its own thresholds in its own config.
func burnLabel(rate, frac float64, ok bool) string {
	if !ok {
		return "burn -"
	}
	if rate < 0 {
		rate = 0
	}
	label := "burn " + strconv.FormatFloat(rate, 'f', 1, 64) + "%/h"
	if rate == 0 {
		return label // nothing is draining, so there is nothing to project
	}
	hours := (1 - frac) * 100 / rate
	if hours < 0 {
		hours = 0
	}
	left := time.Duration(hours * float64(time.Hour))
	return label + "  full in " + shortDur(left.Round(time.Minute))
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
// its burn rate under that, and every window overlaid on one chart at the
// bottom for comparison. Each is its own frame.
func (m model) historyView(height int) string {
	wins := currentWindows(m)
	if len(wins) == 0 {
		return dimStyle.Render("no windows to graph")
	}
	sel := wins[clamp(m.selected, 0, len(wins)-1)]
	span := historySpans[m.spanIdx]

	series, stamps := utilSeries(m.history, sel.Name)
	// The x-axis is the span itself, not the list of readings, so a column
	// always means the same slice of time and a stretch with no reading draws
	// as a gap.
	to := time.Now()
	from := to.Add(-span.dur)
	if len(series) == 0 {
		return titleStyle.Render(sel.Name+" utilization") + "  " + dimStyle.Render(span.label) +
			"\n\n" + dimStyle.Render("no readings in this span — press r to fetch, or s for a longer span")
	}

	chartW := contentWidth(m.width, frameCols)
	frameW := chartW + frameCols
	xFmt := xTimeFormatter(span.dur)

	rateVals, rateStamps := rateSeries(m.history, sel.Name)

	// Height goes to the frames in order of how much each one earns. The
	// utilization chart always draws, the burn rate next, and the overlay last,
	// because the overlay only says something the top chart does not once there
	// is more than one window.
	avail := height - 1 // the summary line under the frames
	utilH, rateH, winH := 0, 0, 0
	switch {
	case avail >= 26:
		rateH, winH = 5, 6
	case avail >= 20:
		rateH, winH = 4, 5
	case avail >= 12:
		rateH = 4
	}
	if len(rateVals) == 0 {
		rateH = 0
	}
	if len(wins) < 2 {
		winH = 0
	}
	used := frameRows
	if rateH > 0 {
		used += rateH + frameRows
	}
	if winH > 0 {
		used += winH + frameRows
	}
	utilH = clamp(avail-used, 4, 16)

	var b strings.Builder

	// Fixed 0..100% scale rather than min/max: the absolute distance to the
	// limit is the point of this graph, so an autoscaled 40-to-42% band would
	// read as a crisis.
	b.WriteString(frame(titleStyle.Render(sel.Name+" utilization")+
		dimStyle.Render("  ·  span "+span.label),
		timeChart(oneLine(series, stamps, loadBand), loadBands[:],
			from, to, chartW, utilH, 0, 1, pct, xFmt),
		frameW) + "\n")

	if rateH > 0 {
		sust := sustainableRate(sel.Name)
		// A rate has no natural ceiling, so take the top from the data. A flat
		// zero series gives top 0, and timeChart then draws every point on the
		// floor, which is correct for a window that is not draining.
		_, top := bounds(rateVals)
		b.WriteString(frame(titleStyle.Render("burn rate, %/h"),
			timeChart(oneLine(rateVals, rateStamps, func(v float64) int {
				return rateBand(v, sust)
			}), loadBands[:], from, to, chartW, rateH, 0, top, pctPerHour, xFmt),
			frameW) + "\n")
	}

	if winH > 0 {
		var lines []chartLine
		var names []string
		for _, w := range wins {
			s, st := utilSeries(m.history, w.Name)
			if len(s) == 0 {
				continue
			}
			// Each window takes one palette entry, so the band function ignores
			// the value and answers with the series index. The index counts the
			// lines drawn, not the windows read, because legend colours by
			// position too. A window with no readings would otherwise shift
			// every line off its own legend dot.
			idx := len(lines) % len(seriesPalette)
			lines = append(lines, chartLine{vals: s, stamps: st,
				band: func(float64) int { return idx }})
			names = append(names, w.Name)
		}
		// The legend rides in the frame title, so the chart keeps the row it
		// would otherwise spend on it.
		b.WriteString(frame(titleStyle.Render("all windows")+"   "+legend(names, seriesPalette),
			timeChart(lines, seriesPalette, from, to, chartW, winH, 0, 1, pct, xFmt),
			frameW) + "\n")
	}

	style := loadStyle(series[len(series)-1])
	min, max := bounds(series)
	b.WriteString(labelStyle.Render("  min ") + valueStyle.Render(pct(min)) +
		labelStyle.Render("  max ") + valueStyle.Render(pct(max)) +
		labelStyle.Render("  now ") + style.Render(pct(series[len(series)-1])) +
		labelStyle.Render("  n=") + valueStyle.Render(itoa(len(series))))
	if rate, ok := burnRate(m.history, sel.Name, time.Now()); ok {
		if rate < 0 {
			// A small dip is accounting noise, not a refund. burnLabel and
			// rateLabel clamp the same value, so this summary must too.
			rate = 0
		}
		b.WriteString(labelStyle.Render("  burn ") +
			valueStyle.Render(strconv.FormatFloat(rate, 'f', 1, 64)+"%/h"))
	}
	b.WriteString(dimStyle.Render("   tab window, s span"))
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

// ---- Tokens tab ------------------------------------------------------------

// tokensView graphs what clusage itself spent on probe calls: a cumulative
// total over the chosen span, the cost of each call, and the breakdown by
// stream with the cached share. Each is its own frame.
func (m model) tokensView(height int) string {
	span := historySpans[m.spanIdx]
	var b strings.Builder

	if len(m.tokens) == 0 {
		b.WriteString(titleStyle.Render("clusage token spend") +
			dimStyle.Render("   span ") + valueStyle.Render(span.label) + "\n\n")
		b.WriteString(dimStyle.Render("no calls in this span — press r to fetch, or s for a longer span") + "\n\n")
		b.WriteString(m.tokenTotalsBlock())
		return clip(b.String(), height)
	}

	cum, per, stamps := tokenSeries(m.tokens)
	chartW := contentWidth(m.width, frameCols)
	frameW := chartW + frameCols
	to := time.Now()
	from := to.Add(-span.dur)
	xFmt := xTimeFormatter(span.dur)
	tokenLabel := func(v float64) string { return commas(int64(v + 0.5)) }

	var spanTotal tokenUse
	for _, s := range m.tokens {
		spanTotal = spanTotal.add(s.Used)
	}
	// A probe call is far under the cache minimum, so both cache rows read
	// zero. A reader needs to know whether the counter is broken or the call is
	// simply too small.
	note := ""
	if spanTotal.CacheRead == 0 && spanTotal.CacheCreate == 0 {
		note = dimStyle.Render(
			"cache rows read 0: a probe call is far under the ~1024 token cache minimum")
	}

	// The breakdown frame holds six rows, and the all-time total one. What is
	// left goes to the two charts, with the per-call chart taking the smaller
	// share: it answers "is a call getting dearer", which needs less height
	// than the shape of the running total.
	reserve := 6 + frameRows + 1
	if note != "" {
		reserve++
	}
	perH := 5
	cumH := clamp(height-reserve-2*frameRows-perH, 4, 14)

	// Spend has no limit to sit under, so a token chart carries one colour.
	// There is no band for a value to fall in.
	spend := []lipgloss.Style{positiveStyle}
	b.WriteString(frame(titleStyle.Render("cumulative spend")+
		dimStyle.Render("  ·  span "+span.label),
		timeChart(oneLine(cum, stamps, solid), spend, from, to,
			chartW, cumH, 0, cum[len(cum)-1], tokenLabel, xFmt),
		frameW) + "\n")

	_, perTop := bounds(per)
	b.WriteString(frame(titleStyle.Render("per call"),
		timeChart(oneLine(per, stamps, solid), spend, from, to,
			chartW, perH, 0, perTop, tokenLabel, xFmt),
		frameW) + "\n")

	body := tokenBreakdown(spanTotal, len(m.tokens))
	if note != "" {
		body += "\n" + note
	}
	b.WriteString(frame(titleStyle.Render("last "+span.label), body, frameW) + "\n")
	b.WriteString(m.tokenTotalsBlock())
	return clip(b.String(), height)
}

// tokenBreakdown lists one row per token stream, plus the cached share and the
// per-call average. The caller frames it, so it carries no title of its own and
// ends without a line break.
func tokenBreakdown(u tokenUse, calls int) string {
	var b strings.Builder
	row := func(name string, v int64) {
		b.WriteString(labelStyle.Render(padRight(name, 12)) +
			valueStyle.Render(padLeft(commas(v), 10)) + "\n")
	}
	row("input", u.Input)
	row("output", u.Output)
	row("cache write", u.CacheCreate)
	row("cache read", u.CacheRead)
	row("total", u.total())
	avg := int64(0)
	if calls > 0 {
		avg = u.total() / int64(calls)
	}
	b.WriteString(labelStyle.Render(padRight("calls", 12)) +
		valueStyle.Render(padLeft(itoa(calls), 10)) +
		labelStyle.Render("   avg ") + valueStyle.Render(commas(avg)+"/call") +
		labelStyle.Render("   cached ") + valueStyle.Render(pct(u.cachedFrac())+" of input"))
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
	frameW := contentWidth(m.width, frameCols) + frameCols

	// rows lays out one "label value" line per pair. The value carries its own
	// styling, so a row can report a state in colour.
	rows := func(pairs ...[2]string) string {
		var lines []string
		for _, p := range pairs {
			lines = append(lines, labelStyle.Render(padRight(p[0], 18))+p[1])
		}
		return strings.Join(lines, "\n")
	}
	val := valueStyle.Render

	var b strings.Builder
	b.WriteString(frame(titleStyle.Render("runtime"), rows(
		[2]string{"source", sourceLabel(m.cfg.Source)},
		[2]string{"model", val(m.cfg.Model)},
		[2]string{"threshold", val(itoa(m.cfg.ThresholdMinutes) + "m")},
		[2]string{"history window", val(itoa(m.cfg.HistoryHours) + "h")},
	), frameW) + "\n")

	var schedule string
	if !cronValid(m.cfg.FetchCron) {
		label := m.cfg.FetchCron
		if strings.TrimSpace(label) == "" {
			label = "(unset)"
		}
		schedule = rows(
			[2]string{"fetch_cron", errorStyle.Render(label) +
				dimStyle.Render("  needs 5 fields per expression, ; separates several")},
			[2]string{"auto-fetch", dimStyle.Render("disabled")},
		)
	} else {
		state := positiveStyle.Render("on")
		if !m.autoFetch {
			state = warnStyle.Render("paused (a resumes)")
		}
		next := dimStyle.Render("never")
		if t, ok := nextFetch(m.cfg, time.Now()); ok {
			next = val(t.Local().Format("Mon 15:04")) +
				dimStyle.Render("  ("+untilLabel(t, time.Now())+")")
		}
		schedule = rows(
			[2]string{"fetch_cron", val(m.cfg.FetchCron)},
			[2]string{"auto-fetch", state},
			[2]string{"next fetch", next},
		)
	}
	b.WriteString(frame(titleStyle.Render("schedule"), schedule, frameW) + "\n")

	// The guard rail runs as a shell hook, and a CLUSAGE_GUARD_* variable
	// overrides the file for one session. Report what the hook would apply,
	// not what the file says, and name the variables that took over.
	g, over := effectiveGuard(m.cfg.Guard)
	guardTitle := titleStyle.Render("guard rail")
	if len(over) > 0 {
		guardTitle += dimStyle.Render("   env: " + strings.Join(over, ", "))
	}
	b.WriteString(frame(guardTitle, rows(
		[2]string{"cuts", val("5h " + itoa(g.Soft5h) + "%   7d " + itoa(g.Hard7d) + "%")},
		[2]string{"check every", val(itoa(g.Interval) + "s idle, " + itoa(g.IntervalMin) +
			"s near a cut, " + itoa(g.Poll) + "s while paused")},
		[2]string{"max wait", val(itoa(g.MaxWait)+"s, then deny") +
			dimStyle.Render("   overage ") + overageLabel(g.AllowOverage)},
		[2]string{"allow tools", val(strings.Join(g.AllowTools, ", "))},
		[2]string{"hook", hookLabel(m.guard)},
	), frameW) + "\n")

	b.WriteString(frame(titleStyle.Render("paths"), rows(
		[2]string{"config file", val(tilde(m.cfgPath))},
		[2]string{"database", val(tilde(m.dbPath))},
		[2]string{"token", tokenLabel(m.hasToken)},
		[2]string{"version", val(version)},
	), frameW) + "\n")

	b.WriteString(dimStyle.Render("  Edit the file and restart to change these. Cron fields: minute hour") + "\n")
	b.WriteString(dimStyle.Render("  day-of-month month day-of-week, e.g. \"*/15 * * * *\" every 15 minutes,") + "\n")
	b.WriteString(dimStyle.Render("  \"0 9-17 * * 1-5\" weekday work hours, \"5 9 * * *;35 18 * * *\" twice a day."))
	return b.String()
}

// overageLabel says whether the guard keeps working once a window is spent.
func overageLabel(allowed bool) string {
	if allowed {
		return warnStyle.Render("allowed")
	}
	return positiveStyle.Render("blocked")
}

// hookLabel says whether the guard is registered with Claude Code, and names
// anything that has stood it down. A registered hook that is switched off
// still denies nothing, so both halves matter.
func hookLabel(st guardStatus) string {
	state := positiveStyle.Render("registered")
	if !st.Registered {
		state = dimStyle.Render("not registered  (clusage hook install)")
	}
	switch {
	case st.Off:
		state += warnStyle.Render("   off switch present")
	case st.Disabled:
		state += warnStyle.Render("   CLUSAGE_GUARD_DISABLE=1")
	}
	return state
}

// tilde shortens a path under the home directory to "~/...". It keeps the row
// inside a narrow terminal, and keeps the user name out of a screenshot.
func tilde(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || !strings.HasPrefix(p, home+string(os.PathSeparator)) {
		return p
	}
	return "~" + p[len(home):]
}

// tokenLabel reports whether a probe can run at all.
func sourceLabel(s string) string {
	if !slices.Contains(sources, s) {
		return warnStyle.Render("not set  (" + strings.Join(sources, ", ") + ")")
	}
	return valueStyle.Render(s)
}

func tokenLabel(has bool) string {
	if has {
		return positiveStyle.Render("found")
	}
	return warnStyle.Render("not found  (see the README)")
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
