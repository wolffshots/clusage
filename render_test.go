package main

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// seedReadings builds a plausible history: 5h climbing to 92%, 7d flat-ish.
func seedReadings(n int) []Reading {
	out := make([]Reading, n)
	base := time.Now().Add(-time.Duration(n) * 20 * time.Minute)
	for i := range out {
		f := 0.05 + 0.87*float64(i)/float64(n-1)
		out[i] = Reading{
			FetchedAt: base.Add(time.Duration(i) * 20 * time.Minute),
			Model:     "claude-opus-5",
			Headers: map[string]string{
				"anthropic-ratelimit-unified-5h-utilization":      ftoa(f),
				"anthropic-ratelimit-unified-5h-status":           "allowed_warning",
				"anthropic-ratelimit-unified-5h-reset":            itoa(int(time.Now().Add(2 * time.Hour).Unix())),
				"anthropic-ratelimit-unified-7d-utilization":      "0.41",
				"anthropic-ratelimit-unified-7d-status":           "allowed",
				"anthropic-ratelimit-unified-7d-reset":            itoa(int(time.Now().Add(70 * time.Hour).Unix())),
				"anthropic-ratelimit-unified-7d-opus-utilization": "0.63",
				"anthropic-ratelimit-unified-7d-opus-status":      "allowed",
			},
		}
	}
	return out
}

func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', 4, 64) }

// TestRenderTabs drives every tab through View at a realistic terminal size and
// prints the result, so a broken layout shows up as a diff rather than only as
// a panic. It also asserts the chrome the layout depends on.
func TestRenderTabs(t *testing.T) {
	// The Config tab reports whether the guard rail hook is registered, which
	// it reads from the Claude Code settings file. Point that at an empty
	// directory so the render does not change with the machine it runs on.
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rs := seedReadings(40)
	m := newModel(nil, Config{
		Model: "claude-opus-5", ThresholdMinutes: 5,
		FetchCron: "*/15 * * * *", HistoryHours: 168,
		Guard: defaultConfig.Guard,
	}, "/tmp/config.json", rs[len(rs)-1], true)
	m.history = rs
	m.tokens = seedTokenSamples(40)
	for _, s := range m.tokens {
		m.tokenTotal = m.tokenTotal.add(s.Used)
	}
	m.tokenCalls = len(m.tokens)

	var mm tea.Model = m
	mm, _ = mm.Update(tea.WindowSizeMsg{Width: 96, Height: 32})

	for i, name := range tabNames {
		mm, _ = mm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{rune('1' + i)}})
		out := mm.View()
		t.Logf("\n===== %s =====\n%s", name, out)
		if strings.Count(out, "\n")+1 > 32 {
			t.Errorf("%s view is %d lines, taller than the 32-row terminal", name, strings.Count(out, "\n")+1)
		}
		if !strings.Contains(out, "cron") {
			t.Errorf("%s view lost the cron marker from the tab bar", name)
		}
	}
}

// TestFetchErrorKeepsTabsReachable checks that a failed fetch reports itself
// without hiding the last good reading. An exhausted limit used to blank every
// tab, which removed the numbers the user needed to see.
func TestFetchErrorKeepsTabsReachable(t *testing.T) {
	rs := seedReadings(40)
	m := newModel(nil, Config{Model: "claude-opus-5", FetchCron: "*/15 * * * *"},
		"/tmp/config.json", rs[len(rs)-1], true)
	m.history = rs

	var mm tea.Model = m
	mm, _ = mm.Update(tea.WindowSizeMsg{Width: 96, Height: 32})
	mm, _ = mm.Update(fetchErrMsg{err: errors.New("429 rate limit exceeded")})

	out := mm.View()
	t.Logf("\n===== Now with a failed fetch =====\n%s", out)
	if !strings.Contains(out, "429 rate limit exceeded") {
		t.Error("the failure is not reported")
	}
	if !strings.Contains(out, "92%") {
		t.Error("the last good reading was hidden by the failure")
	}
	if got := strings.Count(out, "\n") + 1; got > 32 {
		t.Errorf("view is %d lines, taller than the 32-row terminal", got)
	}
	// The banner must not survive a good fetch.
	mm, _ = mm.Update(fetchedMsg{r: rs[len(rs)-1], at: rs[len(rs)-1].FetchedAt})
	if strings.Contains(mm.View(), "429 rate limit exceeded") {
		t.Error("the banner outlived the failure")
	}
}

func TestBurnLabel(t *testing.T) {
	// 67 points of headroom at 14.2 points per hour is about 4h43m.
	got := burnLabel(14.2, 0.33, true)
	if !strings.Contains(got, "burn 14.2%/h") {
		t.Fatalf("burnLabel lost the rate: %q", got)
	}
	if !strings.Contains(got, "full in 4h43m") {
		t.Fatalf("burnLabel lost the projection: %q", got)
	}
	// Nothing draining means no projection to make.
	if got := burnLabel(0, 0.33, true); got != "burn 0.0%/h" {
		t.Fatalf("a zero rate must not project: %q", got)
	}
	if got := burnLabel(-4, 0.33, true); got != "burn 0.0%/h" {
		t.Fatalf("a negative rate must clamp and not project: %q", got)
	}
	if got := burnLabel(0, 0.33, false); got != "burn -" {
		t.Fatalf("an unknown rate must read as a dash: %q", got)
	}
	// A full window has no time left rather than a negative one.
	if got := burnLabel(10, 1.0, true); !strings.Contains(got, "full in 0m") {
		t.Fatalf("a spent window must read as no time left: %q", got)
	}
}

func TestNowViewShowsTheBurnRow(t *testing.T) {
	hist := seedReadings(12)
	m := model{
		width:   100,
		latest:  hist[len(hist)-1],
		hasData: true,
		history: hist,
		cfg:     defaultConfig,
	}
	out := m.nowView(40)
	if !strings.Contains(out, "burn ") {
		t.Fatalf("nowView shows no burn row:\n%s", out)
	}
	if !strings.Contains(out, "%/h") {
		t.Fatalf("nowView shows no rate unit:\n%s", out)
	}
}

func TestNowViewWithoutHistoryShowsADash(t *testing.T) {
	one := seedReadings(2)[:1]
	m := model{
		width:   100,
		latest:  one[0],
		hasData: true,
		history: one,
		cfg:     defaultConfig,
	}
	out := m.nowView(40)
	if !strings.Contains(out, "burn -") {
		t.Fatalf("one reading supports no rate, want a dash:\n%s", out)
	}
}

func TestSustainableRate(t *testing.T) {
	// A 5h window spends 100 points over 5 hours, so 20 points per hour.
	if got := sustainableRate("5h"); got < 19.99 || got > 20.01 {
		t.Fatalf("sustainableRate(5h) = %v; want 20", got)
	}
	// A 7d window sustains a much slower rate.
	if got := sustainableRate("7d"); got < 0.59 || got > 0.6 {
		t.Fatalf("sustainableRate(7d) = %v; want about 0.595", got)
	}
	// A window with no length falls back to the 5h figure.
	if sustainableRate("overage") != sustainableRate("5h") {
		t.Fatal("a nameless window must fall back to the 5h rate")
	}
}

func TestRateBand(t *testing.T) {
	s := sustainableRate("5h") // 20
	if got := rateBand(5, s); got != 0 {
		t.Fatalf("below sustainable must be band 0, got %d", got)
	}
	if got := rateBand(30, s); got != 1 {
		t.Fatalf("under twice sustainable must be band 1, got %d", got)
	}
	if got := rateBand(90, s); got != 2 {
		t.Fatalf("above twice sustainable must be band 2, got %d", got)
	}
	// A rate of zero is the calmest possible reading.
	if got := rateBand(0, s); got != 0 {
		t.Fatalf("zero must be band 0, got %d", got)
	}
}

func TestHistoryViewShowsTheRateChart(t *testing.T) {
	hist := seedReadings(24)
	m := model{
		width:   100,
		latest:  hist[len(hist)-1],
		hasData: true,
		history: hist,
		cfg:     defaultConfig,
	}
	out := m.historyView(40)
	if !strings.Contains(out, "%/h") {
		t.Fatalf("historyView shows no rate chart:\n%s", out)
	}
	if !strings.Contains(out, "utilization") {
		t.Fatalf("historyView lost the utilization chart:\n%s", out)
	}
}

func TestHistoryViewDegradesOnAShortTerminal(t *testing.T) {
	hist := seedReadings(24)
	m := model{
		width:   100,
		latest:  hist[len(hist)-1],
		hasData: true,
		history: hist,
		cfg:     defaultConfig,
	}
	for _, height := range []int{8, 12, 40} {
		out := m.historyView(height)
		if lines := strings.Count(out, "\n") + 1; lines > height {
			t.Fatalf("height %d produced %d lines, which pushes the footer off screen",
				height, lines)
		}
		if !strings.Contains(out, "utilization") {
			t.Fatalf("height %d lost the utilization chart:\n%s", height, out)
		}
	}
}

// flatRateReadings builds a history whose burn rate is exactly constant. Every
// step adds one sixteenth of the window, and a four decimal string carries that
// value without rounding. So every pair reports the same instant rate, down to
// the last bit, and the rate series is genuinely flat.
func flatRateReadings(n int) []Reading {
	out := make([]Reading, n)
	base := time.Now().Add(-time.Duration(n) * 20 * time.Minute)
	for i := range out {
		out[i] = Reading{
			FetchedAt: base.Add(time.Duration(i) * 20 * time.Minute),
			Model:     "claude-opus-5",
			Headers: map[string]string{
				"anthropic-ratelimit-unified-5h-utilization": ftoa(float64(i) / 16),
				"anthropic-ratelimit-unified-5h-status":      "allowed",
			},
		}
	}
	return out
}

// TestHistoryViewKeepsEveryWindow pins the row budget. Every frame must pay for
// its own borders. If one does not, clip eats the overlay frame off the bottom
// of the view and the other windows go with it.
func TestHistoryViewKeepsEveryWindow(t *testing.T) {
	hist := seedReadings(24)
	m := model{
		width:   100,
		latest:  hist[len(hist)-1],
		hasData: true,
		history: hist,
		cfg:     defaultConfig,
	}
	// Height 32 is the tall case. Height 27 is the body a 32 row terminal
	// gives this view, and the budget that undercounted the rate block clipped
	// two sparkline rows there.
	for _, height := range []int{27, 32} {
		out := m.historyView(height)
		if !strings.Contains(out, "burn rate, %/h") {
			t.Fatalf("height %d drew no rate chart, so this case proves nothing:\n%s",
				height, out)
		}
		for _, name := range []string{"5h", "7d", "7d-opus"} {
			if !strings.Contains(out, "● "+name) {
				t.Fatalf("height %d lost %s from the overlay legend:\n%s", height, name, out)
			}
		}
		if lines := strings.Count(out, "\n") + 1; lines > height {
			t.Fatalf("height %d produced %d lines, which pushes the footer off screen:\n%s",
				height, lines, out)
		}
	}
}

// TestHistoryViewDrawsAFlatRateFlat pins the burn rate chart to a zero based
// scale. Scaling between the series own min and max would give a flat rate a
// zero span, and it would then draw as a sawtooth of rounding noise.
//
// A short terminal has to keep the rate chart. It is the reason to look at this
// tab, so it may not be the first frame to go.
func TestHistoryViewDrawsAFlatRateFlat(t *testing.T) {
	hist := flatRateReadings(12)
	m := model{
		width:   100,
		latest:  hist[len(hist)-1],
		hasData: true,
		history: hist,
		cfg:     defaultConfig,
	}
	out := m.historyView(13)
	if !strings.Contains(out, "burn rate") {
		t.Fatalf("height 13 dropped the burn rate frame:\n%s", out)
	}

	// Take the rate frame's own rows, which run from its title to its bottom
	// border, and count how many of them carry ink.
	inked := 0
	inFrame := false
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.Contains(line, "burn rate"):
			inFrame = true
			continue
		case !inFrame:
			continue
		case strings.HasPrefix(line, "╰"):
			inFrame = false
			continue
		}
		if strings.ContainsFunc(line, func(r rune) bool { return r >= '⠁' && r <= '⣿' }) {
			inked++
		}
	}
	if inked != 1 {
		t.Fatalf("a flat rate must draw on one row, got %d:\n%s", inked, out)
	}
}

// TestFramesAlign pins the frame border to one width per view. A title is built
// from styled runes, and lipgloss measures display columns rather than bytes,
// so a mistake here shows up as a ragged right edge rather than as a failure.
func TestFramesAlign(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rs := seedReadings(40)
	m := newModel(nil, defaultConfig, "/tmp/config.json", rs[len(rs)-1], true)
	m.history = rs
	m.tokens = seedTokenSamples(40)

	var mm tea.Model = m
	mm, _ = mm.Update(tea.WindowSizeMsg{Width: 96, Height: 32})
	for i, name := range tabNames {
		mm, _ = mm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{rune('1' + i)}})
		want, seen := 0, 0
		for _, line := range strings.Split(mm.View(), "\n") {
			if !strings.HasPrefix(line, "╭") && !strings.HasPrefix(line, "│") &&
				!strings.HasPrefix(line, "╰") {
				continue
			}
			seen++
			w := lipgloss.Width(line)
			if want == 0 {
				want = w
				continue
			}
			if w != want {
				t.Errorf("%s: frame line %d columns wide, want %d: %q", name, w, want, line)
			}
		}
		if seen == 0 && name != "Now" {
			t.Errorf("%s drew no frames", name)
		}
	}
}
