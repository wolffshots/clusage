package main

import (
	"testing"
	"time"
)

func TestStatuslineHeaders(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)

	// Before the first API response there are no rate limits, and that is not an error.
	if _, ok, err := statuslineHeaders([]byte(`{"model":{"display_name":"Opus"}}`), Reading{}, now); ok || err != nil {
		t.Fatalf("no rate_limits: ok=%v err=%v, want false, nil", ok, err)
	}

	h, ok, err := statuslineHeaders([]byte(`{"rate_limits":{
		"five_hour":{"used_percentage":23.5,"resets_at":1800003600},
		"seven_day":{"used_percentage":41,"resets_at":1800500000}}}`), Reading{}, now)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	ws := parseWindows(h)
	if len(ws) != 2 || ws[0].Name != "5h" || ws[0].Utilization != "0.235" || ws[0].Reset != "1800003600" ||
		ws[1].Name != "7d" || ws[1].Utilization != "0.41" {
		t.Fatalf("windows = %+v", ws)
	}

	// Claude Code drops a window once it resets. A dropped 5h whose reset has
	// passed reads 0%, and a 7d whose reset is still ahead carries forward.
	prev := Reading{Headers: map[string]string{
		"anthropic-ratelimit-unified-5h-utilization": "0.9",
		"anthropic-ratelimit-unified-5h-reset":       "1799999000",
		"anthropic-ratelimit-unified-7d-utilization": "0.41",
		"anthropic-ratelimit-unified-7d-reset":       "1800500000",
	}}
	h, ok, _ = statuslineHeaders([]byte(`{"rate_limits":{"spend_limit":{"used_percentage":5,"resets_at":1801000000}}}`), prev, now)
	if !ok {
		t.Fatal("want a reading")
	}
	got := map[string]window{}
	for _, w := range parseWindows(h) {
		got[w.Name] = w
	}
	if w := got["5h"]; w.Utilization != "0" || w.Reset != "" {
		t.Fatalf("reset 5h = %+v, want 0%% and no reset", w)
	}
	if w := got["7d"]; w.Utilization != "0.41" || w.Reset != "1800500000" {
		t.Fatalf("carried 7d = %+v", w)
	}
	if w := got["spend"]; w.Utilization != "0.05" {
		t.Fatalf("spend = %+v", w)
	}
}
