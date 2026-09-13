package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// statuslineModel marks a reading that came from the Claude Code status line
// rather than from a probe call.
const statuslineModel = "statusline"

// statuslineWindows maps the status line rate_limits keys onto the window names
// the probe headers use, so every view, the history and the hook read both
// sources the same way.
var statuslineWindows = []struct{ key, name string }{
	{"five_hour", "5h"},
	{"seven_day", "7d"},
	{"spend_limit", "spend"},
}

type statuslineInput struct {
	RateLimits map[string]struct {
		UsedPercentage *float64 `json:"used_percentage"`
		ResetsAt       int64    `json:"resets_at"`
	} `json:"rate_limits"`
}

// statuslineHeaders turns the status line JSON into the header map a probe
// reading stores. prev is the last stored reading. ok is false when the input
// carries no rate limits, which is normal before the first API response.
//
// Claude Code drops a window from the input once its resets_at passes. A
// window that is simply missing would read to the hook as no usable 5h row,
// and the hook denies on that. So a missing window whose previous reset has
// passed records 0%, and one whose reset is still ahead carries forward.
func statuslineHeaders(raw []byte, prev Reading, now time.Time) (map[string]string, bool, error) {
	var in statuslineInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, false, err
	}
	if len(in.RateLimits) == 0 {
		return nil, false, nil
	}
	prevWin := map[string]window{}
	for _, w := range parseWindows(prev.Headers) {
		prevWin[w.Name] = w
	}
	h := map[string]string{}
	set := func(name, util, reset string) {
		h["anthropic-ratelimit-unified-"+name+"-utilization"] = util
		if reset != "" {
			h["anthropic-ratelimit-unified-"+name+"-reset"] = reset
		}
	}
	for _, sw := range statuslineWindows {
		if v, ok := in.RateLimits[sw.key]; ok && v.UsedPercentage != nil {
			// The status line reports 0 to 100. The headers carry a 0 to 1 fraction.
			set(sw.name, strconv.FormatFloat(*v.UsedPercentage/100, 'f', -1, 64),
				strconv.FormatInt(v.ResetsAt, 10))
			continue
		}
		p, ok := prevWin[sw.name]
		if !ok {
			continue
		}
		if t, ok := p.resetTime(); ok && !t.After(now) {
			set(sw.name, "0", "")
		} else {
			set(sw.name, p.Utilization, p.Reset)
		}
	}
	return h, len(h) > 0, nil
}

// statusline is the "clusage statusline" command. Claude Code runs it as the
// status line command, hands it the session JSON on stdin, and shows what it
// prints. It stores a reading when the numbers changed, then prints the windows.
func statusline() error {
	// Run by hand, stdin is the terminal and ReadAll would wait for an EOF
	// nobody sends. Say what the command is for instead.
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		return fmt.Errorf(`statusline reads the JSON Claude Code sends on stdin: set "statusLine" in ~/.claude/settings.json to {"type": "command", "command": "clusage statusline"}, see the README`)
	}
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	prev, _, err := latestReading(db)
	if err != nil {
		return err
	}
	now := time.Now()
	h, ok, err := statuslineHeaders(raw, prev, now)
	if err != nil || !ok {
		// Print nothing rather than an error. The status line is on screen the
		// whole session, and an early run without rate limits is expected.
		return err
	}
	// Claude Code reruns the status line on every event, often a few times a
	// second. Only a change, or a minute of quiet, earns a row.
	if !sameHeaders(prev.Headers, h) || now.Sub(prev.FetchedAt) >= time.Minute {
		if err := saveReading(db, Reading{FetchedAt: now, Model: statuslineModel, Headers: h}); err != nil {
			return err
		}
	}
	var parts []string
	for _, w := range parseWindows(h) {
		if f, ok := w.utilFrac(); ok {
			parts = append(parts, fmt.Sprintf("%s %.0f%%", w.Name, f*100))
		}
	}
	fmt.Println(strings.Join(parts, " · "))
	return nil
}

func sameHeaders(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
