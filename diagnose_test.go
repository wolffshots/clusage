package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	tea "github.com/charmbracelet/bubbletea"
)

// fakeLocations replaces the token locations, so a test never reads the
// keychain or a real Claude Code login.
func fakeLocations(t *testing.T, locs ...tokenLocation) {
	t.Helper()
	old := tokenLocations
	t.Cleanup(func() { tokenLocations = old })
	tokenLocations = locs
}

// loginJSON is a Claude Code login that expires at exp.
func loginJSON(token string, exp time.Time, scopes ...string) string {
	return fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"refreshToken":"SECRET-REFRESH","expiresAt":%d,
		"scopes":[%s],"subscriptionType":"max","rateLimitTier":"tier-x"}}`,
		token, exp.UnixMilli(), `"`+strings.Join(scopes, `","`)+`"`)
}

func TestDiagnoseFindsProblemsAndHidesTokens(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CLUSAGE_GUARD_STATE", filepath.Join(t.TempDir(), "stamp"))
	now := time.Now()
	db := memDB(t)
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	run := func(cfg Config) diagnosis { return diagnose(db, cfg, cfgPath, now) }
	has := func(d diagnosis, level int, text string) bool {
		for _, s := range d.Suggestions {
			if s.Level == level && strings.Contains(s.Text, text) {
				return true
			}
		}
		return false
	}

	// An unset source is an error, and nothing else needs a token.
	fakeTokens(t)
	fakeLocations(t)
	if d := run(Config{}); !has(d, sugError, `Set "source"`) {
		t.Fatalf("unset source: %+v", d.Suggestions)
	}

	// A narrow bare token and an expired login: the usage source has no token.
	narrow := "SECRET-NARROW"
	login := loginJSON("SECRET-LOGIN", now.Add(-time.Hour), "user:inference", "user:profile")
	fakeTokens(t, narrow)
	fakeLocations(t,
		tokenLocation{"CLAUDE_CODE_OAUTH_TOKEN", "env", func() (string, error) { return narrow, nil }, false},
		tokenLocation{"the Claude Code credentials file", ".credentials.json", func() (string, error) { return login, nil }, true},
	)
	if err := markNarrow(db, narrow, now); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Source: "usage", Fallback: "probe", Model: "m"}
	d := run(cfg)
	if !has(d, sugWarn, "expired") || !has(d, sugWarn, "No token can read the usage endpoint") {
		t.Fatalf("expired login and narrow token: %+v", d.Suggestions)
	}
	text := d.text()
	if strings.Contains(text, "SECRET") {
		t.Fatalf("the diagnosis shows a secret:\n%s", text)
	}
	for _, want := range []string{"narrow (recorded)", "profile yes", tokenFingerprint(narrow), "subscription     max", "tier-x"} {
		if !strings.Contains(text, want) {
			t.Errorf("diagnosis lacks %q:\n%s", want, text)
		}
	}

	// A login recorded as narrow is the wrong record the README warns about.
	good := loginJSON("SECRET-GOOD", now.Add(5*time.Hour), "user:profile")
	fakeTokens(t, "SECRET-GOOD")
	fakeLocations(t, tokenLocation{"the Claude Code credentials file", ".credentials.json",
		func() (string, error) { return good, nil }, true})
	if err := markNarrow(db, "SECRET-GOOD", now); err != nil {
		t.Fatal(err)
	}
	if d := run(cfg); !has(d, sugError, "delete from narrow_tokens") {
		t.Fatalf("narrow login: %+v", d.Suggestions)
	}
	if _, err := db.Exec(`DELETE FROM narrow_tokens`); err != nil {
		t.Fatal(err)
	}

	// A 429 wait, a clock off by a minute, and the calls table.
	tooMany := &anthropic.Error{StatusCode: http.StatusTooManyRequests,
		Request:  httptest.NewRequest(http.MethodGet, "/api/oauth/usage", nil),
		Response: &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}}}
	if err := saveFetchError(db, "usage", tooMany, now); err != nil {
		t.Fatal(err)
	}
	for _, tr := range []trace{
		{At: now.Add(-2 * time.Minute), Source: "usage", Token: "the Claude Code login", Status: 429, Duration: 80 * time.Millisecond, RequestID: "req_bad"},
		{At: now.Add(-time.Minute), Source: "probe", Token: "the Claude Code login", Status: 200, Duration: 300 * time.Millisecond, RequestID: "req_good", Skew: time.Minute, HasSkew: true},
	} {
		if err := saveTrace(db, tr); err != nil {
			t.Fatal(err)
		}
	}
	d = run(cfg)
	if !has(d, sugInfo, "waiting out a 429") || !has(d, sugWarn, "local clock differs") || !has(d, sugInfo, "failed 1 times") {
		t.Fatalf("wait, skew and probe use: %+v", d.Suggestions)
	}
	text = d.text()
	for _, want := range []string{"last ok", "last failed", "req_good", "req_bad", "claude login", "usage latency"} {
		if !strings.Contains(text, want) {
			t.Errorf("calls lack %q:\n%s", want, text)
		}
	}
	// Errors sort first.
	if d.Suggestions[0].Level < d.Suggestions[len(d.Suggestions)-1].Level {
		t.Errorf("suggestions are not sorted by level: %+v", d.Suggestions)
	}
}

func TestTracesKeepTheLast200(t *testing.T) {
	db := memDB(t)
	for i := range traceKeep + 25 {
		if err := saveTrace(db, trace{At: time.Now(), Source: "probe", Status: 200 + i%2}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := recentTraces(db, 1000)
	if err != nil || len(got) != traceKeep {
		t.Fatalf("kept %d traces, err %v", len(got), err)
	}
}

// Every HTTP call a read makes is traced with its token source and request-id,
// whether it succeeds or not.
func TestReadsAreTraced(t *testing.T) {
	var usageCalls, probeCalls int
	fakeAPI(t, http.StatusTooManyRequests, http.StatusOK, &usageCalls, &probeCalls)
	db := memDB(t)
	if _, _, _, err := readUsage(context.Background(), db, Config{Source: "usage", Fallback: "probe"}, "/c.json", "m"); err != nil {
		t.Fatal(err)
	}
	got, err := recentTraces(db, 10)
	if err != nil || len(got) != 2 {
		t.Fatalf("traces = %+v, %v", got, err)
	}
	probe, usage := got[0], got[1]
	if usage.Source != "usage" || usage.Status != 429 || usage.Token != "token 1" || usage.ok() {
		t.Errorf("usage trace = %+v", usage)
	}
	if probe.Source != "probe" || probe.Status != 200 || !probe.ok() || !probe.HasSkew {
		t.Errorf("probe trace = %+v", probe)
	}
}

func TestReadStatuslineSetup(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	settings := func(cmd string) {
		body := `{}`
		if cmd != "" {
			body = fmt.Sprintf(`{"statusLine": {"type": "command", "command": %q}}`, cmd)
		}
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	script := filepath.Join(dir, "status.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ninput=$(cat)\nprintf '%s' \"$input\" | clusage statusline\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ cmd, want string }{
		{"", "none"},
		{"clusage statusline", "direct"},
		{"/usr/local/bin/clusage statusline", "direct"},
		{"sh " + script, "wrapper"},
		{script, "wrapper"},
		{"ccstatusline", "other"},
	} {
		settings(c.cmd)
		if got := readStatuslineSetup().Mode; got != c.want {
			t.Errorf("command %q: mode %q, want %q", c.cmd, got, c.want)
		}
	}
}

func TestFormatTable(t *testing.T) {
	got := formatTable([][]string{{"a", "bb", "c"}, {"ccc", "d", ""}})
	want := []string{"a    bb  c", "ccc  d"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("formatTable = %q, want %q", got, want)
	}
}

// The Diagnostics tab exists only when config.json turns it on. It loads on
// key 5, fits the terminal, and scrolls within its content.
func TestDiagnosticsTab(t *testing.T) {
	off := testModel("")
	if strings.Contains(off.renderTabs(), "Diagnostics") {
		t.Error("the tab shows without the flag")
	}
	if next, cmd := off.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'5'}}); next.(model).active == viewDiagnostics || cmd != nil {
		t.Error("key 5 opened the tab without the flag")
	}

	m := newModel(nil, Config{Model: "m", Diagnostics: true, HistoryHours: 168}, "/tmp/c.json", Reading{}, false)
	m.width, m.height = 96, 32
	if !strings.Contains(m.renderTabs(), "Diagnostics") {
		t.Error("the tab is missing with the flag on")
	}
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'5'}})
	m = next.(model)
	if m.active != viewDiagnostics || cmd == nil {
		t.Fatal("key 5 did not open and load the tab")
	}
	var lines []string
	for i := range 60 {
		lines = append(lines, fmt.Sprintf("line %02d", i))
	}
	next, _ = m.Update(diagMsg{d: diagnosis{At: time.Now(),
		Suggestions: []suggestion{{sugWarn, "fix the thing"}},
		Sections:    []diagSection{{"Long", lines}}}})
	m = next.(model)

	out := m.View()
	if got := strings.Count(out, "\n") + 1; got > 32 {
		t.Errorf("view is %d lines, taller than the terminal", got)
	}
	if !strings.Contains(out, "fix the thing") || strings.Contains(out, "line 59") {
		t.Errorf("the top of the tab is wrong:\n%s", out)
	}
	for _, k := range []tea.KeyType{tea.KeyEnd, tea.KeyDown} {
		next, _ = m.Update(tea.KeyMsg{Type: k})
		m = next.(model)
	}
	if out := m.View(); !strings.Contains(out, "line 59") || strings.Contains(out, "fix the thing") {
		t.Errorf("end did not scroll to the bottom:\n%s", out)
	}
	before := m.diagOffset
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if next.(model).diagOffset != before {
		t.Error("scrolled past the end")
	}
}
