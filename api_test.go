package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseWindowsAndFormat(t *testing.T) {
	headers := map[string]string{
		"anthropic-ratelimit-unified-5h-status":              "allowed",
		"anthropic-ratelimit-unified-5h-utilization":         "0.61",
		"anthropic-ratelimit-unified-5h-reset":               "1787584800",
		"anthropic-ratelimit-unified-7d-status":              "allowed_warning",
		"anthropic-ratelimit-unified-7d-utilization":         "0.9",
		"anthropic-ratelimit-unified-7d-reset":               "1787587200",
		"anthropic-ratelimit-unified-overage-status":         "rejected",
		"anthropic-ratelimit-unified-overage-utilization":    "1.0",
		"anthropic-ratelimit-unified-status":                 "allowed_warning",
		"anthropic-ratelimit-unified-7d-surpassed-threshold": "0.75",
	}
	got := parseWindows(headers)
	if len(got) != 3 {
		t.Fatalf("want 3 windows, got %d: %+v", len(got), got)
	}
	if got[0].Name != "5h" || got[1].Name != "7d" || got[2].Name != "overage" {
		t.Fatalf("wrong order: %+v", got)
	}
	if got[0].Utilization != "0.61" || got[0].Status != "allowed" {
		t.Fatalf("5h window wrong: %+v", got[0])
	}
	if percentUsed(got[0].Utilization) != "61% used" {
		t.Fatalf("percentUsed: %q", percentUsed(got[0].Utilization))
	}

	now := time.Unix(1787584800, 0).Add(-90 * time.Minute)
	if want := "(in 1h30m)"; !strings.Contains(formatReset("1787584800", now), want) {
		t.Fatalf("formatReset missing %q: %q", want, formatReset("1787584800", now))
	}
	if formatReset("", now) != "" {
		t.Fatal("empty reset must render empty")
	}
	if formatReset("not-a-time", now) != "not-a-time" {
		t.Fatal("unparseable reset must pass through")
	}
}

func TestPercentUsedRoundsToWholePercents(t *testing.T) {
	cases := map[string]string{
		"0.29": "29% used",
		"0.07": "7% used",
		"0.61": "61% used",
		"1.0":  "100% used",
		"":     "",
	}
	for in, want := range cases {
		if got := percentUsed(in); got != want {
			t.Errorf("percentUsed(%q) = %q, want %q", in, got, want)
		}
	}
}

// A rejected call that carries a rate limit header parseWindows cannot use must
// still report the error. Swallowing it persists a window-less reading, which
// then serves as the cache and prints an empty report.
func TestFetchUsageKeepsTheErrorWhenNoWindowParses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("anthropic-ratelimit-unified-status", "allowed")
		w.Header().Set("anthropic-ratelimit-requests-remaining", "0")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"boom"}}`))
	}))
	defer srv.Close()
	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)

	headers, _, err := fetchUsage(context.Background(), "tok", "claude-opus-5")
	if err == nil {
		t.Fatalf("fetchUsage() error = nil, want the API error (headers: %v)", headers)
	}
	if len(headers) != 0 {
		t.Fatalf("fetchUsage() headers = %v, want none", headers)
	}
}

// The happy path still swallows the error, because the response carries windows.
func TestFetchUsageKeepsHeadersWhenAWindowParses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("anthropic-ratelimit-unified-5h-utilization", "0.42")
		w.Header().Set("anthropic-ratelimit-unified-5h-status", "allowed")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
	}))
	defer srv.Close()
	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)

	headers, _, err := fetchUsage(context.Background(), "tok", "claude-opus-5")
	if err != nil {
		t.Fatalf("fetchUsage() error = %v, want nil", err)
	}
	if len(parseWindows(headers)) != 1 {
		t.Fatalf("fetchUsage() headers = %v, want one parsable window", headers)
	}
}

func TestRateLabel(t *testing.T) {
	if got := rateLabel(14.23, true); got != "14.2%/h" {
		t.Fatalf("rateLabel = %q", got)
	}
	if got := rateLabel(0, true); got != "0.0%/h" {
		t.Fatalf("a zero rate must still render: %q", got)
	}
	// A negative rate is nonsense to show, so clamp it.
	if got := rateLabel(-5, true); got != "0.0%/h" {
		t.Fatalf("a negative rate must clamp to zero: %q", got)
	}
	// An unknown rate leaves the column blank, which the hook reads as unknown.
	if got := rateLabel(0, false); got != "" {
		t.Fatalf("an unknown rate must render empty: %q", got)
	}
}

func TestReportPutsTheRateBeforeTheReset(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	pts := []ratePoint{
		{at: now.Add(-20 * time.Minute), frac: 0.30},
		{at: now.Add(-10 * time.Minute), frac: 0.32},
		{at: now.Add(-1 * time.Minute), frac: 0.33},
	}
	hist := readingsFrom("5h", pts)
	latest := hist[len(hist)-1]
	latest.Headers["anthropic-ratelimit-unified-5h-reset"] =
		strconv.FormatInt(now.Add(2*time.Hour).Unix(), 10)

	out := captureStdout(t, func() { report(latest, hist, now, true, false) })
	fields := strings.Fields(strings.Split(out, "\n")[0])
	// name, percent, "used", status, rate, then the reset text.
	if len(fields) < 6 {
		t.Fatalf("too few fields: %q", out)
	}
	if fields[0] != "5h" || fields[3] != "allowed" {
		t.Fatalf("the hook reads $1 and $4, which moved: %q", out)
	}
	if !strings.HasSuffix(fields[4], "%/h") {
		t.Fatalf("want the rate in field 5, got %q in %q", fields[4], out)
	}
	if fields[5] != "resets" {
		t.Fatalf("the reset text must follow the rate: %q", out)
	}
}

func TestReportLeavesTheColumnBlankWithoutHistory(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	r := Reading{FetchedAt: now, Model: "claude-opus-5", Headers: map[string]string{
		"anthropic-ratelimit-unified-5h-utilization": "0.33",
		"anthropic-ratelimit-unified-5h-status":      "allowed",
	}}
	out := captureStdout(t, func() { report(r, []Reading{r}, now, false, false) })
	if strings.Contains(out, "%/h") {
		t.Fatalf("one reading supports no rate, so the column must be blank: %q", out)
	}
}

// TestLoadHistoryFallsBackOnReadFailure covers the readingsSince failure
// path. A closed database gives a genuine readingsSince error, not a
// simulated one. loadHistory must swallow it, warn on stderr in the style
// of the other non-fatal save failures in usage(), and return no history
// instead of an error, so usage() can still call report() on the cached
// path. usage() itself now only ever calls readingsSince through
// loadHistory, so this also covers the path usage() takes.
func TestLoadHistoryFallsBackOnReadFailure(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	db, err := openDB()
	if err != nil {
		t.Fatal(err)
	}
	db.Close() // Any later query on db now fails.

	stderr := captureStderr(t, func() {
		hist := loadHistory(db, time.Now().Add(-7*24*time.Hour))
		if hist != nil {
			t.Fatalf("loadHistory on a closed database = %v, want nil", hist)
		}
	})
	if !strings.Contains(stderr, "clusage:") {
		t.Fatalf("a history read failure must warn like the other non-fatal saves: %q", stderr)
	}

	// The report must still print the window row and leave the rate column
	// blank, exactly as it does for any other reading with no history.
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	r := Reading{FetchedAt: now, Model: "claude-opus-5", Headers: map[string]string{
		"anthropic-ratelimit-unified-5h-utilization": "0.33",
		"anthropic-ratelimit-unified-5h-status":      "allowed",
	}}
	out := captureStdout(t, func() { report(r, nil, now, true, false) })
	if !strings.Contains(out, "5h") || !strings.Contains(out, "allowed") {
		t.Fatalf("a history read failure must not swallow the window row: %q", out)
	}
	if strings.Contains(out, "%/h") {
		t.Fatalf("no history means the rate column must be blank: %q", out)
	}
}

// captureStderr runs fn with os.Stderr redirected and returns what it printed.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = wr
	fn()
	wr.Close()
	os.Stderr = old
	out, err := io.ReadAll(rd)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// captureStdout runs fn with os.Stdout redirected and returns what it printed.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = wr
	fn()
	wr.Close()
	os.Stdout = old
	out, err := io.ReadAll(rd)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}
