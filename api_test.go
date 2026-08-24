package main

import (
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
