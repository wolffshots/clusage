package main

import (
	"strconv"
	"strings"
	"time"
)

// cronDue reports whether a schedule selects the minute containing t. The schedule is one or more cron expressions separated by ";",
// as one expression cannot always cover a schedule (for example a fetch at
// 18:05 but not at 18:35). An empty or invalid schedule never matches, which
// disables auto-fetching.
func cronDue(sched string, t time.Time) bool {
	for _, expr := range strings.Split(sched, ";") {
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
	for i, f := range fields {
		if fieldMatches(f, vals[i], cronBounds[i][0], cronBounds[i][1]) {
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

// cronBounds is the range a bare "*" covers in each field.
var cronBounds = [5][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6}}

// fieldMatches reports whether v is selected by one cron field. min and max are
// the range a bare "*" covers.
func fieldMatches(field string, v, min, max int) bool {
	for _, part := range strings.Split(field, ",") {
		lo, hi, step, ok := parsePart(part, min, max)
		if !ok {
			return false
		}
		if v >= lo && v <= hi && (v-lo)%step == 0 {
			return true
		}
	}
	return false
}

// parsePart parses one comma-separated element of a cron field into an
// inclusive range and a step. min and max are the range a bare "*" covers. ok
// is false for a part that no value can ever match, which includes a
// three-letter name such as MON or JAN, as this parser takes numbers only.
func parsePart(part string, min, max int) (lo, hi, step int, ok bool) {
	rng := part
	step = 1
	if base, s, found := strings.Cut(part, "/"); found {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			return 0, 0, 0, false
		}
		rng, step = base, n
	}
	lo, hi = min, max
	if rng != "*" {
		a, b, hasRange := strings.Cut(rng, "-")
		n, err := strconv.Atoi(a)
		if err != nil {
			return 0, 0, 0, false
		}
		lo, hi = n, n
		if hasRange {
			n, err = strconv.Atoi(b)
			if err != nil {
				return 0, 0, 0, false
			}
			hi = n
		} else if step > 1 {
			// "n/step" runs from n to the end of the field, as real cron does.
			hi = max
		}
	}
	if lo < min || hi > max || lo > hi {
		return 0, 0, 0, false
	}
	return lo, hi, step, true
}

// cronValid reports whether every expression in a schedule has five fields that
// a value can actually match, so the config view can flag a typo instead of
// silently never firing. It runs each field through the same parser cronMatches
// uses, so a value out of range, a reversed range or a name such as MON is
// invalid.
func cronValid(sched string) bool {
	if strings.TrimSpace(sched) == "" {
		return false
	}
	for _, expr := range strings.Split(sched, ";") {
		fields := strings.Fields(expr)
		if len(fields) != 5 {
			return false
		}
		for i, f := range fields {
			max := cronBounds[i][1]
			if i == 4 {
				max = 7 // Sunday is written as either 0 or 7.
			}
			for _, part := range strings.Split(f, ",") {
				if _, _, _, ok := parsePart(part, cronBounds[i][0], max); !ok {
					return false
				}
			}
		}
	}
	return true
}

// nextFetch scans forward minute by minute for the next match, up to a week
// ahead. It returns ok=false for a schedule that never fires.
func nextFetch(sched string, from time.Time) (time.Time, bool) {
	t := from.Truncate(time.Minute).Add(time.Minute)
	for i := 0; i < 7*24*60; i++ {
		if cronDue(sched, t) {
			return t, true
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}, false
}
