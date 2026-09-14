package main

import (
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
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
		if !cronDue(cfg.FetchCron, at(when)) {
			t.Errorf("cronDue(%s) = false, want true", when)
		}
	}
	for _, when := range []string{"2026-08-24 09:35", "2026-08-24 18:05", "2026-08-24 12:00"} {
		if cronDue(cfg.FetchCron, at(when)) {
			t.Errorf("cronDue(%s) = true, want false", when)
		}
	}
	// An empty schedule must never fire, so auto-fetch stays off by default.
	if cronDue("", at("2026-08-24 09:05")) {
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
	got, ok := nextFetch("*/15 * * * *", at("2026-08-24 10:01"))
	if !ok || !got.Equal(at("2026-08-24 10:15")) {
		t.Errorf("nextFetch = %v (ok=%v), want 10:15", got, ok)
	}
	// The current minute is excluded, so a fetch that just fired reports the
	// following slot instead of the one it already handled.
	got, ok = nextFetch("*/15 * * * *", at("2026-08-24 10:15"))
	if !ok || !got.Equal(at("2026-08-24 10:30")) {
		t.Errorf("nextFetch at a matching minute = %v, want 10:30", got)
	}
	if _, ok := nextFetch("", at("2026-08-24 10:00")); ok {
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

// probe_cron alone arms the tick chain, and its minute starts a fetch even
// where fetch_cron selects nothing.
func TestProbeCronFires(t *testing.T) {
	m := testModel("")
	m.cfg.ProbeCron = "0 6 * * *"
	m = newModel(nil, m.cfg, "/tmp/c.json", Reading{FetchedAt: time.Now()}, true)
	m.width, m.height = 96, 32
	if !m.autoFetch || !m.keys.Auto.Enabled() {
		t.Fatal("a valid probe_cron did not arm auto-fetch")
	}
	next, _ := m.Update(cronTickMsg{t: at("2026-08-24 06:00"), seq: m.cronSeq})
	if !next.(model).fetching {
		t.Error("the probe minute did not start a fetch")
	}
	next, _ = m.Update(cronTickMsg{t: at("2026-08-24 06:01"), seq: m.cronSeq})
	if next.(model).fetching {
		t.Error("a minute neither schedule selects started a fetch")
	}
	if !strings.Contains(m.renderTabs(), "probe 0 6 * * *") || !strings.Contains(m.configView(), "0 6 * * *   next ") {
		t.Error("the probe schedule is not shown")
	}
}

// A probe minute that lands while a fetch is in flight waits for that fetch,
// rather than being lost, because the probe is what starts a 5h window.
func TestProbeWaitsForAFetchInFlight(t *testing.T) {
	m := newModel(nil, Config{Model: "m", ProbeCron: "0 6 * * *", HistoryHours: 168},
		"/tmp/c.json", Reading{FetchedAt: time.Now()}, true)
	m.fetching = true
	next, _ := m.Update(cronTickMsg{t: at("2026-08-24 06:00"), seq: m.cronSeq})
	m = next.(model)
	if !m.probePending {
		t.Fatal("the probe minute was dropped while a fetch was in flight")
	}
	next, cmd := m.Update(fetchedMsg{r: m.latest, at: time.Now()})
	m = next.(model)
	if m.probePending || !m.fetching || cmd == nil {
		t.Errorf("the waiting probe did not start: pending=%v fetching=%v", m.probePending, m.fetching)
	}
	// It runs once. A second tick in the same minute starts nothing more.
	m.fetching = false
	next, _ = m.Update(cronTickMsg{t: at("2026-08-24 06:00").Add(20 * time.Second), seq: m.cronSeq})
	if next.(model).fetching || next.(model).probePending {
		t.Error("the probe minute fired twice")
	}
}

// The error count reloads on its own timer, so failures from the guard rail
// hook show up between TUI fetches, even with the schedules paused.
func TestErrorsTickRearms(t *testing.T) {
	m := testModel("")
	m.autoFetch = false
	if _, cmd := m.Update(errorsTickMsg{}); cmd == nil {
		t.Error("the error count tick did not re-arm")
	}
}

// Failed reads from every run show as a total on the tab bar and a breakdown
// on the Config tab.
func TestErrorCountsShow(t *testing.T) {
	m := testModel("* * * * *")
	if strings.Contains(m.renderTabs(), "errors") {
		t.Error("the tab bar reports errors before any")
	}
	next, _ := m.Update(errorsMsg{counts: []errorCount{{"probe", 0, 1}, {"usage", 429, 2}}})
	m = next.(model)
	if !strings.Contains(m.renderTabs(), "3 errors 24h") {
		t.Errorf("tab bar = %q", m.renderTabs())
	}
	if cv := m.configView(); !strings.Contains(cv, "usage 429×2") || !strings.Contains(cv, "probe error×1") {
		t.Errorf("config view lacks the breakdown:\n%s", cv)
	}
}

// A scheduled fetch that hands back the reading already on screen says so.
func TestScheduledFetchMarksUnchanged(t *testing.T) {
	m := testModel("* * * * *")
	next, _ := m.Update(fetchedMsg{r: m.latest, auto: true, at: time.Now()})
	if got := next.(model).lastAuto; !strings.HasSuffix(got, "✓ unchanged") {
		t.Errorf("same reading: lastAuto = %q", got)
	}
	next, _ = m.Update(fetchedMsg{r: Reading{FetchedAt: time.Now().Add(time.Second)}, auto: true, at: time.Now()})
	if got := next.(model).lastAuto; !strings.HasSuffix(got, "✓") {
		t.Errorf("new reading: lastAuto = %q", got)
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

func TestVersionFlag(t *testing.T) {
	// The Homebrew formula's smoke test runs "clusage --version", so it must
	// succeed without a token, a config file, a database, or a TTY.
	for _, flag := range []string{"--version", "-version"} {
		if err := run([]string{flag}); err != nil {
			t.Errorf("run(%q) = %v, want no error", flag, err)
		}
	}
	// The default must stay a real value: the release build overwrites it, and
	// an empty string would make the formula's assert_match pass on nothing.
	if version == "" {
		t.Error("version is empty")
	}
}

// Every way of asking for help prints text and never reaches the command, so
// usage -h works with no source set and --help does not open the TUI.
func TestHelp(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cases := map[string]string{
		"--help": "Getting started", "-h": "Commands:", "help": "Commands:",
		"help usage": "-force", "usage -h": "-threshold", "usage --help": "-force",
		"statusline -h": "statusLine", "hook status --help": "uninstall", "help setup": "CLAUDE_CODE_OAUTH_TOKEN",
	}
	for args, want := range cases {
		out := captureStdout(t, func() {
			if err := run(strings.Fields(args)); err != nil {
				t.Errorf("run(%q) = %v", args, err)
			}
		})
		if !strings.Contains(out, want) {
			t.Errorf("run(%q) printed no %q:\n%s", args, want, out)
		}
	}
	if err := run([]string{"help", "nope"}); err == nil {
		t.Error("help for an unknown command returned no error")
	}
	// Each command the dispatch accepts has its own help.
	for _, cmd := range []string{"tui", "usage", "statusline", "setup", "hook", "guard-config", "help"} {
		if _, ok := commandHelp[cmd]; !ok {
			t.Errorf("no help for %q", cmd)
		}
	}
}

func TestUnknownCommandIsReported(t *testing.T) {
	// The formula also asserts on this message, so it must name every command.
	err := run([]string{"nope"})
	if err == nil {
		t.Fatal("an unknown command returned no error")
	}
	for _, want := range []string{"tui", "setup", "usage"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

func TestSaveTokenRejectsLineBreak(t *testing.T) {
	// The keychain prompt reads one line for the value and one for the
	// confirmation, so an embedded line break would store a truncated token and
	// still report success. The guard runs before the command is started, so
	// this test never touches the real keychain.
	for _, bad := range []string{"abc\ndef", "abc\r\ndef", "abc\r", "\n"} {
		if err := saveToken(bad); err == nil {
			t.Errorf("saveToken(%q) was accepted; it would store a truncated token", bad)
		}
	}
}

// TestTimeChartGap checks the line chart's two promises: the x-axis covers the
// whole window, and a stretch with no reading stays blank instead of drawing a
// straight line across the hole.
func TestTimeChartGap(t *testing.T) {
	to := time.Now()
	from := to.Add(-4 * time.Hour)
	var vals []float64
	var stamps []time.Time
	// Two runs of readings, one at each end, with nothing in the middle two hours.
	for i := 0; i < 6; i++ {
		vals = append(vals, 0.2)
		stamps = append(stamps, from.Add(time.Duration(i)*10*time.Minute))
	}
	for i := 0; i < 6; i++ {
		vals = append(vals, 0.8)
		stamps = append(stamps, from.Add(3*time.Hour+time.Duration(i)*10*time.Minute))
	}

	out := timeChart(oneLine(vals, stamps, loadBand), loadBands[:], from, to, 60, 12, 0, 1,
		pct, xTimeFormatter(4*time.Hour))
	rows := strings.Split(out, "\n")
	if len(rows) < 6 {
		t.Fatalf("timeChart drew %d rows, want the height it was given", len(rows))
	}

	// Column 30 of 60 sits in the hole, so no graph row may carry ink there.
	// The last two rows are the axis and its labels, so stop before them.
	for i, row := range rows[:len(rows)-2] {
		r := []rune(row)
		if len(r) <= 30 {
			continue
		}
		if r[30] != ' ' {
			t.Errorf("row %d drew %q in the hole, want a gap", i, string(r[30]))
		}
	}
	// Ink must still exist on both sides of the hole.
	if !strings.ContainsAny(out, "⠁⠂⠄⡀⢀⠈⠐⠠⠉⠒⠤⣀⠊⠔⠃⠋⣀⠒⠓⠛⠿⣿") {
		t.Error("timeChart drew no braille at all")
	}
}

// TestSeriesPaletteIsDistinct pins the overlay chart palette. A shade of the
// text colour renders as near-white on a dark terminal, so a line drawn with it
// reads as uncoloured while the legend still advertises a colour.
func TestSeriesPaletteIsDistinct(t *testing.T) {
	seen := map[lipgloss.TerminalColor]int{}
	for i, s := range seriesPalette {
		c := s.GetForeground()
		if c == fg || c == dim {
			t.Errorf("palette entry %d uses a text colour, which draws as uncoloured", i)
		}
		if c == positive || c == warnColor || c == negative {
			t.Errorf("palette entry %d uses a threshold colour, which reads as a verdict", i)
		}
		seen[c]++
	}
	if len(seen) != len(seriesPalette) {
		t.Errorf("palette has %d colours for %d entries, so two windows share a line colour",
			len(seen), len(seriesPalette))
	}
}

// TestTimeChartColorsSurviveAGap pins the styling of a segment that opens after
// a hole. Such a segment keeps the band it had before the hole, and a rule that
// styled a data set only on a band change left it on the library default, which
// emits no colour at all and draws uncolored.
//
// The check walks every braille rune and asks which colour was active on it.
// Counting the colours present is not enough: an unstyled segment adds no code,
// so the set of colours still looks correct while half the line draws white.
//
// The colour profile has to be forced. A test run has no TTY, so lipgloss
// strips every escape code and each style renders identically.
func TestTimeChartColorsSurviveAGap(t *testing.T) {
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(old)

	to := time.Now()
	from := to.Add(-4 * time.Hour)
	var vals []float64
	var stamps []time.Time
	// Two runs at the same low utilization, so both sit in band 0 and the whole
	// line must come out one colour. The two hour hole between them breaks the
	// line without changing the band.
	for _, offset := range []time.Duration{0, 10, 20, 30, 180, 190, 200, 210} {
		vals = append(vals, 0.2)
		stamps = append(stamps, from.Add(offset*time.Minute))
	}

	out := timeChart(oneLine(vals, stamps, loadBand), loadBands[:], from, to, 60, 12, 0, 1,
		pct, xTimeFormatter(4*time.Hour))

	sgr := regexp.MustCompile("^\\x1b\\[[0-9;]*m")
	code := func(s lipgloss.Style) string {
		return regexp.MustCompile("\\x1b\\[[0-9;]*m").FindString(s.Render("x"))
	}
	want := code(positiveStyle) // band 0, where the whole series sits

	// Walk the rendered chart, tracking which colour is in force, and record the
	// colour every braille rune is drawn in.
	active := ""
	inked := map[string]int{}
	for i := 0; i < len(out); {
		if m := sgr.FindString(out[i:]); m != "" {
			if m == "\x1b[0m" {
				active = ""
			} else {
				active = m
			}
			i += len(m)
			continue
		}
		r, size := utf8.DecodeRuneInString(out[i:])
		if r >= 0x2800 && r <= 0x28FF && r != 0x2800 {
			inked[active]++
		}
		i += size
	}

	if len(inked) == 0 {
		t.Fatal("the chart drew no braille, so this case proves nothing")
	}
	if len(inked) != 1 || inked[want] == 0 {
		t.Errorf("braille drawn in %v, want every rune in the band 0 green %q", inked, want)
	}
}

// TestBinToColumns pins the reducer the charts draw through. One point per
// column, the highest value in the column, the real time that value was read,
// and no point at all for a column with no reading.
func TestBinToColumns(t *testing.T) {
	from := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	col := time.Hour
	peak := from.Add(20 * time.Minute)
	stamps := []time.Time{
		from,                       // column 0
		peak,                       // column 0, the highest
		from.Add(40 * time.Minute), // column 0
		from.Add(3 * time.Hour),    // column 3, so 1 and 2 stay empty
	}
	vals, out := binToColumns([]float64{0.2, 0.9, 0.4, 0.5}, stamps, from, col, 4)

	if len(vals) != 2 {
		t.Fatalf("binToColumns kept %d points, want one per filled column", len(vals))
	}
	if vals[0] != 0.9 {
		t.Errorf("column 0 kept %v, want the 0.9 peak", vals[0])
	}
	if !out[0].Equal(peak) {
		t.Errorf("column 0 reports %v, want the time the peak was read, %v", out[0], peak)
	}
	if vals[1] != 0.5 {
		t.Errorf("column 3 kept %v, want 0.5", vals[1])
	}
	// The empty columns must not appear as points. The gap between what is left
	// is what breaks the line.
	if got := out[1].Sub(out[0]); got != 2*time.Hour+40*time.Minute {
		t.Errorf("kept points are %v apart, want the hole preserved", got)
	}

	// A reading before the window start clamps into the first column rather than
	// vanishing, and a zero column width passes the series through untouched.
	if v, _ := binToColumns([]float64{1}, []time.Time{from.Add(-time.Hour)}, from, col, 4); len(v) != 1 {
		t.Error("a reading before the window start was dropped")
	}
	if v, _ := binToColumns([]float64{1, 2}, []time.Time{from, from}, from, 0, 4); len(v) != 2 {
		t.Error("a zero column width must pass the series through")
	}
}
