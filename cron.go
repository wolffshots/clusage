package main

import (
	"strconv"
	"strings"
	"time"
)

// fetchDue reports whether the configured schedule selects the minute
// containing t. The schedule is one or more cron expressions separated by ";",
// as one expression cannot always cover a schedule (for example a fetch at
// 18:05 but not at 18:35). An empty or invalid schedule never matches, which
// disables auto-fetching.
func (c Config) fetchDue(t time.Time) bool {
	for _, expr := range strings.Split(c.FetchCron, ";") {
		if cronMatches(expr, t) {
			return true
		}
	}
	return false
}

// cronMatches evaluates a 5-field cron expression (minute hour day-of-month
// month day-of-week) against t. Each field accepts a comma-separated list of
// "*", "n", "a-b", and step forms ("*/n", "a-b/n"). Day-of-week takes 0 or 7
// for Sunday.
//
// ponytail: day-of-month and day-of-week are ANDed. Standard cron ORs them when
// both are restricted. Fix that if anyone writes such an expression.
func cronMatches(expr string, t time.Time) bool {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return false
	}
	vals := [5]int{t.Minute(), t.Hour(), t.Day(), int(t.Month()), int(t.Weekday())}
	bounds := [5][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6}}
	for i, f := range fields {
		if fieldMatches(f, vals[i], bounds[i][0], bounds[i][1]) {
			continue
		}
		// Sunday is written as either 0 or 7.
		if i == 4 && vals[i] == 0 && fieldMatches(f, 7, 0, 7) {
			continue
		}
		return false
	}
	return true
}

// fieldMatches reports whether v is selected by one cron field. min and max are
// the range a bare "*" covers.
func fieldMatches(field string, v, min, max int) bool {
	for _, part := range strings.Split(field, ",") {
		rng, step := part, 1
		if base, s, ok := strings.Cut(part, "/"); ok {
			n, err := strconv.Atoi(s)
			if err != nil || n < 1 {
				return false
			}
			rng, step = base, n
		}
		lo, hi := min, max
		if rng != "*" {
			a, b, hasRange := strings.Cut(rng, "-")
			n, err := strconv.Atoi(a)
			if err != nil {
				return false
			}
			lo, hi = n, n
			if hasRange {
				n, err = strconv.Atoi(b)
				if err != nil {
					return false
				}
				hi = n
			}
		}
		if v >= lo && v <= hi && (v-lo)%step == 0 {
			return true
		}
	}
	return false
}

// cronValid reports whether every expression in a schedule has five fields, so
// the config view can flag a typo instead of silently never firing.
func cronValid(sched string) bool {
	if strings.TrimSpace(sched) == "" {
		return false
	}
	for _, expr := range strings.Split(sched, ";") {
		if len(strings.Fields(expr)) != 5 {
			return false
		}
	}
	return true
}

// nextFetch scans forward minute by minute for the next match, up to a week
// ahead. It returns ok=false for a schedule that never fires.
func nextFetch(c Config, from time.Time) (time.Time, bool) {
	t := from.Truncate(time.Minute).Add(time.Minute)
	for i := 0; i < 7*24*60; i++ {
		if c.fetchDue(t) {
			return t, true
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}, false
}
