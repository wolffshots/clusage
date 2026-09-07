package main

import (
	"context"
	"net/http"
	"net/http/httptest"
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
