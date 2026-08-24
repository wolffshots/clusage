package main

import (
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

// loadStyle colours a utilization fraction green under 60%, amber under 85%,
// red above. The same thresholds drive the gauges and the history lines so a
// colour means one thing across the whole UI.
func loadStyle(frac float64) lipgloss.Style {
	switch {
	case frac >= 0.85:
		return negativeStyle
	case frac >= 0.60:
		return warnStyle
	default:
		return positiveStyle
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
	rows := areaChart(series, chartW, chartH, 0, 1)
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
		b.WriteString(dimStyle.Render(padLeft(axis, 4)) + " " + style.Render(row) + "\n")
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
			line = loadStyle(s[len(s)-1]).Render(sparkline(s, chartW))
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
