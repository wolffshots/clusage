package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClaudeCodeToken(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	write := func(body string) {
		if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	later := time.Now().Add(time.Hour).UnixMilli()
	earlier := time.Now().Add(-time.Hour).UnixMilli()

	if _, err := claudeCodeToken(); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing file: err = %v, want ErrNotExist", err)
	}
	write(fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"fake-access","refreshToken":"fake-refresh","expiresAt":%d}}`, later))
	if tok, err := claudeCodeToken(); err != nil || tok != "fake-access" {
		t.Fatalf("valid login: got %q, %v", tok, err)
	}
	write(fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"fake-access","expiresAt":%d}}`, earlier))
	if _, err := claudeCodeToken(); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired login: err = %v", err)
	}
	write(`{}`)
	if _, err := claudeCodeToken(); err == nil {
		t.Fatal("no login: want an error")
	}
	write(`{"claudeAiOauth":`)
	if _, err := claudeCodeToken(); err == nil || strings.Contains(err.Error(), "fake") {
		t.Fatalf("bad JSON: err = %v", err)
	}
	// The macOS keychain hands back the same JSON with a trailing newline.
	kc := fmt.Sprintf("{\"claudeAiOauth\":{\"accessToken\":\"fake-kc\",\"expiresAt\":%d}}\n", later)
	if tok, err := claudeCodeLogin([]byte(kc), "keychain"); err != nil || tok != "fake-kc" {
		t.Fatalf("keychain login: got %q, %v", tok, err)
	}
}

func windowsByName(h map[string]string) map[string]window {
	out := map[string]window{}
	for _, w := range parseWindows(h) {
		out[w.Name] = w
	}
	return out
}

func TestOAuthUsageHeadersFlatBuckets(t *testing.T) {
	h, err := oauthUsageHeaders([]byte(`{
		"five_hour": {"utilization": 23, "resets_at": "2026-09-13T20:00:00.123456+00:00"},
		"seven_day": {"utilization": 41.5, "resets_at": "2026-09-18T08:00:00+00:00"},
		"seven_day_opus": null,
		"extra_usage": {"is_enabled": true, "utilization": 12}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	w := windowsByName(h)
	if w["5h"].Utilization != "0.23" || w["7d"].Utilization != "0.415" {
		t.Fatalf("5h/7d = %+v %+v", w["5h"], w["7d"])
	}
	if _, ok := w["5h"].resetTime(); !ok {
		t.Fatalf("5h reset %q does not parse", w["5h"].Reset)
	}
	// A null bucket is a window with no use in it, not a missing window.
	if w["7d-opus"].Utilization != "0" {
		t.Fatalf("null 7d-opus = %+v, want 0", w["7d-opus"])
	}
	if w["overage"].Utilization != "0.12" {
		t.Fatalf("overage = %+v", w["overage"])
	}
}

func TestOAuthUsageHeadersLimitsArray(t *testing.T) {
	h, err := oauthUsageHeaders([]byte(`{"limits": [
		{"kind": "session", "percent": 61, "resets_at": "2026-09-13T20:00:00Z"},
		{"kind": "weekly_all", "percent": 30, "resets_at": "2026-09-18T08:00:00Z"},
		{"kind": "weekly_scoped", "percent": 63, "resets_at": "2026-09-18T08:00:00Z",
		 "scope": {"model": {"display_name": "Claude Opus 4.8"}}},
		{"kind": "weekly_scoped", "percent": 0, "scope": {"model": {"display_name": "Sonnet"}}}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	w := windowsByName(h)
	if w["5h"].Utilization != "0.61" || w["7d"].Utilization != "0.3" || w["7d-opus"].Utilization != "0.63" {
		t.Fatalf("windows = %+v", w)
	}
	// A 0% entry with no reset is a placeholder.
	if _, ok := w["7d-sonnet"]; ok {
		t.Fatalf("placeholder became a window: %+v", w["7d-sonnet"])
	}
}

// In auto, a refused usage endpoint and no status line reading fall through to
// the probe, and the error of every step is kept when all of them fail.
func TestReadUsageFallsBackToTheProbe(t *testing.T) {
	probed := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/api/oauth/usage") {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
			return
		}
		probed++
		w.Header().Set("anthropic-ratelimit-unified-5h-utilization", "0.42")
		w.Header().Set("anthropic-ratelimit-unified-7d-utilization", "0.1")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"m","type":"message","role":"assistant","content":[],"model":"x",
			"stop_reason":"max_tokens","usage":{"input_tokens":23,"output_tokens":0}}`))
	}))
	defer srv.Close()
	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "tok")
	// An in-memory database rather than openDB, whose file DSN does not open
	// on Windows. Only the readings table is needed.
	db, err := sql.Open("sqlite", "file::memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1) // every connection to :memory: is a separate database
	if _, err := db.Exec(`CREATE TABLE readings (id INTEGER PRIMARY KEY AUTOINCREMENT,
		fetched_at TEXT NOT NULL, model TEXT NOT NULL, headers TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}

	// No source set is an error that names every choice, and calls nothing.
	for _, src := range []string{"", "token"} {
		_, _, _, err := readUsage(context.Background(), db, Config{Source: src}, "/c.json", "m")
		if err == nil || !strings.Contains(err.Error(), "statusline, usage, probe, auto") || probed != 0 {
			t.Fatalf("source %q: err=%v probed=%d", src, err, probed)
		}
	}

	cfg := Config{Source: "auto", ThresholdMinutes: 5}
	r, used, fresh, err := readUsage(context.Background(), db, cfg, "/c.json", "claude-haiku-4-5")
	if err != nil || !fresh || probed != 1 || used.Input != 23 {
		t.Fatalf("err=%v fresh=%v probed=%d used=%+v", err, fresh, probed, used)
	}
	if windowsByName(r.Headers)["5h"].Utilization != "0.42" {
		t.Fatalf("reading = %+v", r)
	}

	// A fresh status line reading wins over the probe.
	saveStatusline(t, db, time.Now())
	if r, _, fresh, err = readUsage(context.Background(), db, cfg, "/c.json", "m"); err != nil || fresh || r.Model != statuslineModel || probed != 1 {
		t.Fatalf("err=%v fresh=%v model=%q probed=%d", err, fresh, r.Model, probed)
	}

	// The explicit statusline source keeps a stale reading that auto skips.
	saveStatusline(t, db, time.Now().Add(-time.Hour))
	if _, _, fresh, err = readUsage(context.Background(), db, cfg, "/c.json", "m"); err != nil || !fresh || probed != 2 {
		t.Fatalf("auto on a stale reading: err=%v fresh=%v probed=%d", err, fresh, probed)
	}
	cfg.Source = "statusline"
	if _, _, fresh, err = readUsage(context.Background(), db, cfg, "/c.json", "m"); err != nil || fresh {
		t.Fatalf("statusline on a stale reading: err=%v fresh=%v", err, fresh)
	}

	cfg.Source = "usage"
	if _, _, _, err = readUsage(context.Background(), db, cfg, "/c.json", "m"); err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("usage source error = %v", err)
	}

	cfg.Source = "probe"
	if _, _, fresh, err = readUsage(context.Background(), db, cfg, "/c.json", "m"); err != nil || !fresh || probed != 3 {
		t.Fatalf("probe source: err=%v fresh=%v probed=%d", err, fresh, probed)
	}
}

func saveStatusline(t *testing.T, db *sql.DB, at time.Time) {
	t.Helper()
	if err := saveReading(db, Reading{FetchedAt: at, Model: statuslineModel, Headers: map[string]string{
		"anthropic-ratelimit-unified-5h-utilization": "0.5",
	}}); err != nil {
		t.Fatal(err)
	}
}
