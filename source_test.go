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

	"github.com/anthropics/anthropic-sdk-go"
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
	fakeTokens(t, "tok")
	db := memDB(t)

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

// memDB is an in-memory database rather than openDB, whose file DSN does not
// open on Windows.
func memDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file::memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1) // every connection to :memory: is a separate database
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	return db
}

// fakeAPI answers the usage endpoint with usageStatus and the probe with
// probeStatus, where 200 carries windows. It counts the calls to each.
func fakeAPI(t *testing.T, usageStatus, probeStatus int, usageCalls, probeCalls *int) {
	t.Helper()
	errBody := `{"type":"error","error":{"type":"error","message":"no"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/api/oauth/usage") {
			*usageCalls++
			if usageStatus != http.StatusOK {
				w.Header().Set("retry-after", "120")
				w.WriteHeader(usageStatus)
				w.Write([]byte(errBody))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"five_hour":{"utilization":10},"seven_day":{"utilization":20}}`))
			return
		}
		*probeCalls++
		if probeStatus != http.StatusOK {
			w.WriteHeader(probeStatus)
			w.Write([]byte(errBody))
			return
		}
		w.Header().Set("anthropic-ratelimit-unified-5h-utilization", "0.42")
		w.Header().Set("anthropic-ratelimit-unified-7d-utilization", "0.1")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"m","type":"message","role":"assistant","content":[],"model":"x",
			"stop_reason":"max_tokens","usage":{"input_tokens":23,"output_tokens":0}}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)
	// The environment is for TestGuardHookEndToEnd, whose clusage runs in a
	// process of its own.
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "tok")
	fakeTokens(t, "tok")
}

// fakeTokens replaces the token sources with fixed tokens, tried in order, so
// a test never reads the keychain or a real Claude Code login.
func fakeTokens(t *testing.T, toks ...string) {
	t.Helper()
	old := tokenSources
	t.Cleanup(func() { tokenSources = old })
	tokenSources = nil
	for i, tok := range toks {
		tokenSources = append(tokenSources, tokenSource{
			where: fmt.Sprintf("token %d", i+1),
			load:  func() (string, error) { return tok, nil },
		})
	}
}

// A token from claude setup-token lacks user:profile, and the usage endpoint
// answers it 403. The usage source then tries the next token, such as the
// Claude Code login. The probe keeps the first token, which it can use.
func TestUsageTriesTheNextTokenOnAScopeError(t *testing.T) {
	var usageTokens, probeTokens []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if strings.HasSuffix(r.URL.Path, "/api/oauth/usage") {
			usageTokens = append(usageTokens, token)
			if token == "narrow" {
				w.WriteHeader(http.StatusForbidden)
				w.Write([]byte(`{"type":"error","error":{"type":"permission_error",
					"message":"OAuth token does not meet scope requirement user:profile",
					"details":{"required_scopes":["user:profile"],"error_code":"oauth_scope_insufficient"}}}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"five_hour":{"utilization":35},"seven_day":{"utilization":89}}`))
			return
		}
		probeTokens = append(probeTokens, token)
		w.Header().Set("anthropic-ratelimit-unified-5h-utilization", "0.35")
		w.Header().Set("anthropic-ratelimit-unified-7d-utilization", "0.89")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"m","type":"message","role":"assistant","content":[],"model":"x",
			"stop_reason":"max_tokens","usage":{"input_tokens":23,"output_tokens":0}}`))
	}))
	defer srv.Close()
	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)
	ctx := context.Background()

	// Both tokens present: the narrow one is refused, the wide one reads.
	fakeTokens(t, "narrow", "wide")
	db := memDB(t)
	r, _, _, err := readUsage(ctx, db, Config{Source: "usage"}, "/c.json", "m")
	if err != nil || r.Model != usageAPIModel || strings.Join(usageTokens, ",") != "narrow,wide" {
		t.Fatalf("err=%v model=%q usage tokens=%v", err, r.Model, usageTokens)
	}
	if counts, _ := fetchErrorCounts(db, time.Now().Add(-time.Hour)); len(counts) != 0 {
		t.Fatalf("a read that succeeded recorded an error: %+v", counts)
	}
	if _, _, _, err := readUsage(ctx, db, Config{Source: "probe"}, "/c.json", "m"); err != nil || strings.Join(probeTokens, ",") != "narrow" {
		t.Fatalf("probe: err=%v probe tokens=%v, want the first token only", err, probeTokens)
	}

	// Only the narrow token: the read fails, and the error leads with the
	// missing scope and the fix, then names where the token came from.
	fakeTokens(t, "narrow")
	db = memDB(t)
	_, _, _, err = readUsage(ctx, db, Config{Source: "usage"}, "/c.json", "m")
	if err == nil {
		t.Fatal("want an error")
	}
	first, rest, _ := strings.Cut(err.Error(), "\n")
	if !strings.Contains(first, "user:profile") || !strings.Contains(first, "Log in with claude") || !strings.Contains(rest, "token 1: ") {
		t.Fatalf("error = %v", err)
	}
	if counts, _ := fetchErrorCounts(db, time.Now().Add(-time.Hour)); len(counts) != 1 || counts[0].Status != http.StatusForbidden {
		t.Fatalf("counts = %+v", counts)
	}
	if _, _, _, err := readUsage(ctx, db, Config{Source: "probe"}, "/c.json", "m"); err != nil {
		t.Fatalf("probe with the narrow token: %v", err)
	}
}

// With no token anywhere, a read says where to get one. A source that fails to
// load, such as an expired login, names itself and does not stop the next one.
func TestTokensSkipAndName(t *testing.T) {
	old := tokenSources
	t.Cleanup(func() { tokenSources = old })
	tokenSources = []tokenSource{
		{"empty", func() (string, error) { return "", nil }},
		{"expired", func() (string, error) { return "", errors.New("has expired") }},
	}
	var ts tokens
	if _, err := ts.use(func(string) error { return nil }, nil); err == nil || !strings.Contains(err.Error(), "expired: has expired") {
		t.Fatalf("err = %v", err)
	}
	if _, where, err := firstToken(); where != "" || err == nil {
		t.Fatalf("firstToken: where=%q err=%v", where, err)
	}
	tokenSources = append(tokenSources, tokenSource{"good", func() (string, error) { return "t", nil }})
	if tok, where, err := firstToken(); tok != "t" || where != "good" || err != nil {
		t.Fatalf("firstToken = %q %q %v", tok, where, err)
	}
	tokenSources = tokenSources[:1]
	if _, err := (&tokens{}).use(func(string) error { return nil }, nil); !errors.Is(err, errNoToken) {
		t.Fatalf("no token: err = %v", err)
	}
}

// A usage source with a probe fallback probes on a 429, records the 429, and
// waits out its retry-after before it calls the usage endpoint again.
func TestReadUsageFallbackOn429(t *testing.T) {
	var usageCalls, probeCalls int
	fakeAPI(t, http.StatusTooManyRequests, http.StatusOK, &usageCalls, &probeCalls)
	db := memDB(t)
	cfg := Config{Source: "usage", Fallback: "probe", ThresholdMinutes: 5}

	r, _, fresh, err := readUsage(context.Background(), db, cfg, "/c.json", "m")
	if err != nil || !fresh || r.Model != "m" || usageCalls != 1 || probeCalls != 1 {
		t.Fatalf("err=%v fresh=%v model=%q usage=%d probe=%d", err, fresh, r.Model, usageCalls, probeCalls)
	}
	if got := readingSource(r); got != "probe m, fallback" {
		t.Fatalf("readingSource = %q", got)
	}
	counts, err := fetchErrorCounts(db, time.Now().Add(-time.Hour))
	if err != nil || len(counts) != 1 || counts[0] != (errorCount{"usage", 429, 1}) {
		t.Fatalf("counts = %+v, %v", counts, err)
	}
	if until, ok := backoffUntil(db, "usage"); !ok || time.Until(until) < 100*time.Second {
		t.Fatalf("backoff until %v (ok=%v), want about 120s ahead", until, ok)
	}

	// Inside the wait the endpoint is not called, and the skip is no new error.
	if _, _, _, err := readUsage(context.Background(), db, cfg, "/c.json", "m"); err != nil || usageCalls != 1 || probeCalls != 2 {
		t.Fatalf("err=%v usage=%d probe=%d", err, usageCalls, probeCalls)
	}
	if counts, _ := fetchErrorCounts(db, time.Now().Add(-time.Hour)); errorTotal(counts) != 1 {
		t.Fatalf("the skip was counted: %+v", counts)
	}

	// With no fallback the skip is the error.
	cfg.Fallback = ""
	if _, _, _, err := readUsage(context.Background(), db, cfg, "/c.json", "m"); err == nil || !strings.Contains(err.Error(), "waiting out a 429") {
		t.Fatalf("err = %v", err)
	}

	cfg.Fallback = "auto"
	if _, _, _, err := readUsage(context.Background(), db, cfg, "/c.json", "m"); err == nil || !strings.Contains(err.Error(), "unknown fallback") {
		t.Fatalf("auto fallback: err = %v", err)
	}
}

// A chain that ends on a 401 leads its error with the cause and the fix,
// because the guard rail hook shows that first line to the agent.
func TestReadUsageUnauthorized(t *testing.T) {
	var usageCalls, probeCalls int
	fakeAPI(t, http.StatusUnauthorized, http.StatusUnauthorized, &usageCalls, &probeCalls)
	db := memDB(t)
	cfg := Config{Source: "usage", Fallback: "probe"}
	_, _, _, err := readUsage(context.Background(), db, cfg, "/c.json", "m")
	if err == nil {
		t.Fatal("want an error")
	}
	first, _, _ := strings.Cut(err.Error(), "\n")
	if !strings.HasPrefix(first, "the API rejected the OAuth token (401") || !strings.Contains(first, "Run claude") {
		t.Fatalf("first line = %q", first)
	}
	if !strings.Contains(err.Error(), "usage: ") || !strings.Contains(err.Error(), "probe: ") {
		t.Fatalf("the detail lost a step: %v", err)
	}
	if counts, _ := fetchErrorCounts(db, time.Now().Add(-time.Hour)); errorTotal(counts) != 2 {
		t.Fatalf("counts = %+v", counts)
	}
}

// A status line row newer than a usage reading must not stand in for it,
// because it lacks the Opus and overage windows the guard rail checks.
func TestCachedReadingMatchesTheSource(t *testing.T) {
	db := memDB(t)
	if err := saveReading(db, Reading{FetchedAt: time.Now().Add(-time.Minute), Model: usageAPIModel,
		Headers: map[string]string{"anthropic-ratelimit-unified-7d-opus-utilization": "1"}}); err != nil {
		t.Fatal(err)
	}
	saveStatusline(t, db, time.Now())

	cfg := Config{Source: "usage"}
	if r, ok, err := latestReadingFrom(db, cfg.readingModels("m")...); err != nil || !ok || r.Model != usageAPIModel {
		t.Fatalf("usage: got %q ok=%v err=%v", r.Model, ok, err)
	}
	cfg.Source = "auto"
	if r, _, _ := latestReadingFrom(db, cfg.readingModels("m")...); r.Model != statuslineModel {
		t.Fatalf("auto: got %q, want the newest of any source", r.Model)
	}
	// The status line command reads its previous row through latestReading.
	// Losing it breaks the carry-forward of a window Claude Code dropped.
	if r, ok, err := latestReading(db); err != nil || !ok || r.Model != statuslineModel {
		t.Fatalf("latestReading: got %q ok=%v err=%v", r.Model, ok, err)
	}
	cfg = Config{Source: "probe", Fallback: "statusline"}
	if got := cfg.readingModels("m"); len(got) != 2 || got[0] != "m" || got[1] != statuslineModel {
		t.Fatalf("probe with a statusline fallback: %v", got)
	}
}

// Without a retry-after header the wait doubles with each 429 of the last hour
// and stops at maxBackoff. A header wins. Rows past errorKeep are deleted.
func TestBackoffGrowsAndOldErrorsGo(t *testing.T) {
	db := memDB(t)
	now := time.Now()
	tooMany := func() error {
		return &anthropic.Error{StatusCode: http.StatusTooManyRequests,
			Request:  httptest.NewRequest(http.MethodGet, "/api/oauth/usage", nil),
			Response: &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}}}
	}
	for i, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, maxBackoff, maxBackoff} {
		if err := saveFetchError(db, "usage", tooMany(), now); err != nil {
			t.Fatal(err)
		}
		if until, ok := backoffUntil(db, "usage"); !ok || until.Sub(now) != want {
			t.Fatalf("429 number %d: wait %v, want %v", i+1, until.Sub(now), want)
		}
	}
	withHeader := tooMany().(*anthropic.Error)
	withHeader.Response.Header.Set("retry-after", "30")
	if err := saveFetchError(db, "usage", withHeader, now); err != nil {
		t.Fatal(err)
	}
	if until, _ := backoffUntil(db, "usage"); until.Sub(now) != 30*time.Second {
		t.Fatalf("retry-after: wait %v, want 30s", until.Sub(now))
	}

	// A write a week later deletes every row above.
	if err := saveFetchError(db, "probe", errors.New("timeout"), now.Add(errorKeep+time.Minute)); err != nil {
		t.Fatal(err)
	}
	if counts, _ := fetchErrorCounts(db, time.Time{}); errorTotal(counts) != 1 {
		t.Fatalf("old rows kept: %+v", counts)
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
