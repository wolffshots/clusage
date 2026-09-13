package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// An idle session repeats its last numbers on every status line run. A repeat
// must not store a row, or its fresh timestamp passes stale numbers off as new
// to auto.
func TestStatuslineStoresOnlyChanges(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	run := func(input string) {
		t.Helper()
		f, err := os.CreateTemp(t.TempDir(), "stdin")
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString(input)
		f.Seek(0, 0)
		old := os.Stdin
		os.Stdin = f
		defer func() { os.Stdin = old; f.Close() }()
		captureStdout(t, func() {
			if err := statusline(); err != nil {
				t.Fatal(err)
			}
		})
	}
	rows := func() int {
		t.Helper()
		db, err := openDB()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var n int
		db.QueryRow(`SELECT COUNT(*) FROM readings WHERE model = ?`, statuslineModel).Scan(&n)
		return n
	}
	a := `{"rate_limits":{"five_hour":{"used_percentage":12,"resets_at":4000000000}}}`

	run(a)
	if n := rows(); n != 1 {
		t.Fatalf("first run: %d rows, want 1", n)
	}
	// Age the row past the old once-a-minute restate, then put a usage reading
	// on top. Neither may make the same numbers count as new.
	db, err := openDB()
	if err != nil {
		t.Fatal(err)
	}
	db.Exec(`UPDATE readings SET fetched_at = ?`, time.Now().Add(-10*time.Minute).UTC().Format(time.RFC3339Nano))
	saveReading(db, Reading{FetchedAt: time.Now(), Model: usageAPIModel, Headers: map[string]string{
		"anthropic-ratelimit-unified-5h-utilization": "0.2",
	}})
	db.Close()
	run(a)
	if n := rows(); n != 1 {
		t.Fatalf("repeat: %d rows, want 1", n)
	}
	run(strings.Replace(a, "12", "13", 1))
	if n := rows(); n != 2 {
		t.Fatalf("change: %d rows, want 2", n)
	}
}

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
