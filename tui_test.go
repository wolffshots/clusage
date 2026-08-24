package main

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func at(spec string) time.Time {
	t, err := time.Parse("2006-01-02 15:04", spec)
	if err != nil {
		panic(err)
	}
	return t
}

func TestCronMatches(t *testing.T) {
	cases := []struct {
		expr string
		when string
		want bool
	}{
		{"*/15 * * * *", "2026-08-24 10:00", true},
		{"*/15 * * * *", "2026-08-24 10:15", true},
		{"*/15 * * * *", "2026-08-24 10:16", false},
		{"0 9-17 * * 1-5", "2026-08-24 09:00", true},  // Monday
		{"0 9-17 * * 1-5", "2026-08-24 18:00", false}, // past the hour range
		{"0 9-17 * * 1-5", "2026-08-23 09:00", false}, // Sunday
		{"0 12 * * 0", "2026-08-23 12:00", true},      // Sunday as 0
		{"0 12 * * 7", "2026-08-23 12:00", true},      // Sunday as 7
		{"5 9 * * *", "2026-08-24 09:05", true},
		{"5 9 * * *", "2026-08-24 09:35", false},
		{"0 0 1 1 *", "2026-01-01 00:00", true},
		{"0 0 1 1 *", "2026-02-01 00:00", false},
		{"*/0 * * * *", "2026-08-24 10:00", false}, // zero step is invalid
		{"* * * *", "2026-08-24 10:00", false},     // too few fields
		{"", "2026-08-24 10:00", false},
		{"bad * * * *", "2026-08-24 10:00", false},
	}
	for _, c := range cases {
		if got := cronMatches(c.expr, at(c.when)); got != c.want {
			t.Errorf("cronMatches(%q, %s) = %v, want %v", c.expr, c.when, got, c.want)
		}
	}
}

func TestFetchDueMultipleExpressions(t *testing.T) {
	cfg := Config{FetchCron: "5 9 * * *;35 18 * * *"}
	for _, when := range []string{"2026-08-24 09:05", "2026-08-24 18:35"} {
		if !cfg.fetchDue(at(when)) {
			t.Errorf("fetchDue(%s) = false, want true", when)
		}
	}
	for _, when := range []string{"2026-08-24 09:35", "2026-08-24 18:05", "2026-08-24 12:00"} {
		if cfg.fetchDue(at(when)) {
			t.Errorf("fetchDue(%s) = true, want false", when)
		}
	}
	// An empty schedule must never fire, so auto-fetch stays off by default.
	if (Config{}).fetchDue(at("2026-08-24 09:05")) {
		t.Error("empty schedule fired")
	}
}

func TestCronValid(t *testing.T) {
	for _, s := range []string{"* * * * *", "5 9 * * *;35 18 * * *"} {
		if !cronValid(s) {
			t.Errorf("cronValid(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "  ", "* * * *", "* * * * * *", "* * * * *;bad"} {
		if cronValid(s) {
			t.Errorf("cronValid(%q) = true, want false", s)
		}
	}
}

func TestNextFetch(t *testing.T) {
	got, ok := nextFetch(Config{FetchCron: "*/15 * * * *"}, at("2026-08-24 10:01"))
	if !ok || !got.Equal(at("2026-08-24 10:15")) {
		t.Errorf("nextFetch = %v (ok=%v), want 10:15", got, ok)
	}
	// The current minute is excluded, so a fetch that just fired reports the
	// following slot instead of the one it already handled.
	got, ok = nextFetch(Config{FetchCron: "*/15 * * * *"}, at("2026-08-24 10:15"))
	if !ok || !got.Equal(at("2026-08-24 10:30")) {
		t.Errorf("nextFetch at a matching minute = %v, want 10:30", got)
	}
	if _, ok := nextFetch(Config{FetchCron: ""}, at("2026-08-24 10:00")); ok {
		t.Error("nextFetch on an empty schedule reported a time")
	}
}

func TestGauge(t *testing.T) {
	cases := []struct {
		frac float64
		want string
	}{
		{0, "░░░░░░░░░░"},
		{0.5, "█████░░░░░"},
		{1, "██████████"},
		{1.4, "██████████"}, // over the limit still renders full, not wider
		{-0.2, "░░░░░░░░░░"},
	}
	for _, c := range cases {
		if got := gauge(c.frac, 10); got != c.want {
			t.Errorf("gauge(%v, 10) = %q, want %q", c.frac, got, c.want)
		}
	}
	if got := gauge(0.5, 0); got != "" {
		t.Errorf("gauge with zero width = %q, want empty", got)
	}
}

func TestSparkline(t *testing.T) {
	if got := sparkline([]float64{0, 1}, 2); got != "▁█" {
		t.Errorf("sparkline = %q, want ▁█", got)
	}
	// A flat series has no span; every point must land on the lowest level
	// rather than divide by zero.
	if got := sparkline([]float64{5, 5, 5}, 3); got != "▁▁▁" {
		t.Errorf("flat sparkline = %q, want ▁▁▁", got)
	}
	if got := sparkline(nil, 5); got != "" {
		t.Errorf("empty sparkline = %q, want empty", got)
	}
	// More points than width samples down to exactly width glyphs.
	if got := sparkline([]float64{0, 1, 2, 3, 4, 5, 6, 7}, 4); len([]rune(got)) != 4 {
		t.Errorf("resampled sparkline = %q, want 4 glyphs", got)
	}
}

func TestAreaChart(t *testing.T) {
	rows := areaChart([]float64{0, 0.5, 1}, 3, 4, 0, 1)
	if len(rows) != 4 {
		t.Fatalf("got %d rows, want 4", len(rows))
	}
	for i, r := range rows {
		if n := len([]rune(r)); n != 3 {
			t.Errorf("row %d has %d cells, want 3", i, n)
		}
	}
	// Top row: only the full-height column is filled. Bottom row: the zero
	// column is blank and both others are full.
	if rows[0] != "  █" {
		t.Errorf("top row = %q, want %q", rows[0], "  █")
	}
	if rows[3] != " ██" {
		t.Errorf("bottom row = %q, want %q", rows[3], " ██")
	}
	// Fewer points than width right-align, keeping "now" against the axis.
	rows = areaChart([]float64{1}, 4, 2, 0, 1)
	if !strings.HasSuffix(rows[0], "█") || !strings.HasPrefix(rows[0], "   ") {
		t.Errorf("short series row = %q, want right-aligned", rows[0])
	}
	if areaChart([]float64{1}, 0, 2, 0, 1) != nil {
		t.Error("zero width should render nothing")
	}
	// A degenerate scale must not divide by zero.
	if rows := areaChart([]float64{5}, 2, 2, 3, 3); len(rows) != 2 {
		t.Error("zero-span scale broke the chart")
	}
}

func TestUtilSeries(t *testing.T) {
	reading := func(min int, util string) Reading {
		return Reading{
			FetchedAt: at("2026-08-24 10:00").Add(time.Duration(min) * time.Minute),
			Headers: map[string]string{
				"anthropic-ratelimit-unified-5h-utilization": util,
				"anthropic-ratelimit-unified-7d-utilization": "0.25",
				"anthropic-ratelimit-unified-5h-status":      "allowed",
			},
		}
	}
	rs := []Reading{reading(0, "0.1"), reading(5, "bad"), reading(10, "0.4")}
	vals, stamps := utilSeries(rs, "5h")
	if len(vals) != 2 || vals[0] != 0.1 || vals[1] != 0.4 {
		t.Errorf("vals = %v, want [0.1 0.4] (the unparseable reading skipped)", vals)
	}
	if len(stamps) != len(vals) {
		t.Errorf("got %d stamps for %d values", len(stamps), len(vals))
	}
	if vals, _ := utilSeries(rs, "nope"); len(vals) != 0 {
		t.Errorf("unknown window returned %v", vals)
	}
}

func TestLoadStyleThresholds(t *testing.T) {
	// Compare the configured foreground rather than the rendered string: in a
	// non-TTY test run lipgloss strips the colour and every style renders alike.
	cases := []struct {
		frac float64
		want lipgloss.TerminalColor
	}{
		{0, positive}, {0.59, positive},
		{0.60, warnColor}, {0.84, warnColor},
		{0.85, negative}, {1.2, negative},
	}
	for _, c := range cases {
		if got := loadStyle(c.frac).GetForeground(); got != c.want {
			t.Errorf("loadStyle(%v) foreground = %v, want %v", c.frac, got, c.want)
		}
	}
}

// testModel is a model with data already loaded, for driving Update directly.
func testModel(cron string) model {
	m := newModel(nil, Config{Model: "m", FetchCron: cron, HistoryHours: 168},
		"/tmp/c.json", Reading{FetchedAt: time.Now()}, true)
	m.width, m.height = 96, 32
	return m
}

func TestCronTickFiresOncePerMinute(t *testing.T) {
	m := testModel("*/15 * * * *")
	due := at("2026-08-24 10:15")

	// A matching minute starts a fetch.
	next, cmd := m.Update(cronTickMsg{t: due, seq: m.cronSeq})
	m = next.(model)
	if !m.fetching {
		t.Fatal("a matching minute did not start a fetch")
	}
	if cmd == nil {
		t.Error("the tick chain was not re-armed")
	}

	// A second tick inside the same minute must not fire again, even once the
	// first fetch has landed.
	m.fetching = false
	next, _ = m.Update(cronTickMsg{t: due.Add(20 * time.Second), seq: m.cronSeq})
	if next.(model).fetching {
		t.Error("the same minute fired twice")
	}

	// A non-matching minute does nothing but keep ticking.
	next, cmd = m.Update(cronTickMsg{t: at("2026-08-24 10:16"), seq: m.cronSeq})
	if next.(model).fetching {
		t.Error("a non-matching minute fired a fetch")
	}
	if cmd == nil {
		t.Error("a non-matching tick ended the chain")
	}
}

func TestCronTickStaleAndPaused(t *testing.T) {
	m := testModel("* * * * *")

	// A tick from a superseded chain must die rather than re-arm alongside the
	// live chain, or repeated pausing would multiply the tick rate.
	m.cronSeq = 2
	next, cmd := m.Update(cronTickMsg{t: at("2026-08-24 10:00"), seq: 1})
	if next.(model).fetching || cmd != nil {
		t.Error("a stale tick was acted on")
	}

	// A tick that arrives after a pause must end the chain.
	m.autoFetch = false
	next, cmd = m.Update(cronTickMsg{t: at("2026-08-24 10:00"), seq: m.cronSeq})
	if next.(model).fetching || cmd != nil {
		t.Error("a tick after a pause was acted on")
	}
}

func TestAutoToggleBumpsSequence(t *testing.T) {
	m := testModel("* * * * *")
	if !m.autoFetch {
		t.Fatal("a valid schedule should start armed")
	}
	// Pause.
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m = next.(model)
	if m.autoFetch || m.cronTick() != nil {
		t.Error("pausing left the chain armed")
	}
	// Resume: the sequence must advance so the pre-pause tick is stale.
	before := m.cronSeq
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m = next.(model)
	if !m.autoFetch || cmd == nil {
		t.Error("resuming did not re-arm the chain")
	}
	if m.cronSeq == before {
		t.Error("resuming did not bump the sequence, so old ticks stay live")
	}
}

func TestInvalidCronDisablesAuto(t *testing.T) {
	m := testModel("nonsense")
	if m.autoFetch {
		t.Error("an unparseable schedule left auto-fetch on")
	}
	if m.cronTick() != nil {
		t.Error("an unparseable schedule armed a tick chain")
	}
	if m.keys.Auto.Enabled() {
		t.Error("the a binding should be hidden when there is nothing to toggle")
	}
	if !strings.Contains(m.configView(), "needs 5 fields") {
		t.Error("the config tab does not flag the bad schedule")
	}
}

func TestScheduledFailureKeepsLastReading(t *testing.T) {
	m := testModel("* * * * *")
	m.fetching = true
	// A scheduled fetch failing must leave the numbers on screen and only mark
	// the tab bar, otherwise the display blanks unattended.
	next, _ := m.Update(fetchErrMsg{err: errTest, auto: true})
	m = next.(model)
	if m.err != nil {
		t.Error("a scheduled failure took over the body")
	}
	if !strings.Contains(m.renderTabs(), "✗") {
		t.Error("a scheduled failure left no marker on the tab bar")
	}
	// A manual fetch failing is the user's own action, so it reports loudly.
	m.fetching = true
	next, _ = m.Update(fetchErrMsg{err: errTest, auto: false})
	if next.(model).err == nil {
		t.Error("a manual failure was swallowed")
	}
}

func TestManualFetchDoesNotRace(t *testing.T) {
	m := testModel("* * * * *")
	m.fetching = true
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	if cmd != nil {
		t.Error("r started a second fetch while one was in flight")
	}
}

func TestSpanCycleWraps(t *testing.T) {
	m := testModel("")
	seen := map[int]bool{}
	for range historySpans {
		seen[m.spanIdx] = true
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
		m = next.(model)
	}
	if len(seen) != len(historySpans) {
		t.Errorf("s visited %d of %d spans", len(seen), len(historySpans))
	}
}

func TestHistorySpanCappedByConfig(t *testing.T) {
	m := testModel("")
	m.cfg.HistoryHours = 12
	for i := range historySpans {
		m.spanIdx = i
		if got := m.historySpan(); got > 12*time.Hour {
			t.Errorf("span %s = %v, past the 12h config cap", historySpans[i].label, got)
		}
	}
}

var errTest = errTestType{}

type errTestType struct{}

func (errTestType) Error() string { return "boom" }
