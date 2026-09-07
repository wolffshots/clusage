package main

import "testing"

// A schedule that can never fire must be reported as invalid, so the config
// view flags the typo instead of enabling an auto-fetch that never runs.
func TestCronValidRejectsUnfirable(t *testing.T) {
	cases := []struct {
		sched string
		want  bool
	}{
		{"*/15 * * * MON", false},        // day names are not supported
		{"0 9 * * MON", false},           // day names are not supported
		{"75 * * * *", false},            // minute above 59
		{"0 24 * * *", false},            // hour above 23
		{"abc def ghi jkl mno", false},   // five fields, none of them numbers
		{"50-10 * * * *", false},         // reversed range
		{"*/0 * * * *", false},           // zero step
		{"0 0 0 * *", false},             // day-of-month below 1
		{"* * * * *;75 * * * *", false},  // one bad expression fails the set
		{"0 9 * * 1", true},              //
		{"5/15 * * * *", true},           //
		{"0 12 * * 7", true},             // Sunday written as 7
		{"0,30 9-17 1-15 1,6 1-5", true}, //
	}
	for _, c := range cases {
		if got := cronValid(c.sched); got != c.want {
			t.Errorf("cronValid(%q) = %v, want %v", c.sched, got, c.want)
		}
	}
}

// cronValid must agree with nextFetch on schedules that repeat inside a week.
func TestCronValidAgreesWithNextFetch(t *testing.T) {
	scheds := []string{
		"*/15 * * * MON",
		"75 * * * *",
		"0 24 * * *",
		"abc def ghi jkl mno",
		"50-10 * * * *",
		"0 9 * * 1",
		"5/15 * * * *",
	}
	for _, sched := range scheds {
		_, ok := nextFetch(Config{FetchCron: sched}, at("2026-08-24 10:00"))
		if valid := cronValid(sched); valid != ok {
			t.Errorf("cronValid(%q) = %v but nextFetch ok = %v", sched, valid, ok)
		}
	}
}

// "n/step" with no range runs from n to the end of the field, as real cron
// does. Minute "5/15" selects 5, 20, 35 and 50.
func TestCronStepFromBareStart(t *testing.T) {
	for _, when := range []string{
		"2026-08-24 10:05", "2026-08-24 10:20",
		"2026-08-24 10:35", "2026-08-24 10:50",
	} {
		if !cronMatches("5/15 * * * *", at(when)) {
			t.Errorf("cronMatches(\"5/15 * * * *\", %s) = false, want true", when)
		}
	}
	for _, when := range []string{
		"2026-08-24 10:00", "2026-08-24 10:04",
		"2026-08-24 10:15", "2026-08-24 10:51",
	} {
		if cronMatches("5/15 * * * *", at(when)) {
			t.Errorf("cronMatches(\"5/15 * * * *\", %s) = true, want false", when)
		}
	}
	// A step with an explicit range still stops at the end of the range.
	if cronMatches("5-20/15 * * * *", at("2026-08-24 10:35")) {
		t.Error("cronMatches(\"5-20/15 * * * *\", 10:35) = true, want false")
	}
}
