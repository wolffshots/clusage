package main

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
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
	rs := seedReadings(40)
	m := newModel(nil, Config{
		Model: "claude-opus-5", ThresholdMinutes: 5,
		FetchCron: "*/15 * * * *", HistoryHours: 168,
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
