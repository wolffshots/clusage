package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These cases port hooks/clusage-guard.test.sh, which checked the bash hook
// against fixed usage tables.

const (
	fxLow = `5h  33% used  allowed  resets Wed 19:30 (in 4h36m)
7d  20% used  allowed  resets Mon 18:00 (in 123h6m)
overage 78% used allowed_warning`
	fxLowRate = `5h  33% used  allowed  14.2%/h  resets Wed 19:30 (in 4h36m)
7d  20% used  allowed  0.9%/h  resets Mon 18:00 (in 123h6m)
overage 78% used allowed_warning`
	fxHigh5 = `5h  94% used  allowed_warning  resets Wed 19:30 (in 4h36m)
7d  20% used  allowed`
	fxHigh7 = `5h  33% used  allowed
7d  96% used  allowed_warning`
	fxBurned = `5h  100% used  rejected  resets Wed 19:30 (in 4h36m)
7d  60% used  allowed
overage 12% used allowed`
	fxHigh5Rate = `5h  94% used  allowed_warning  12.0%/h  resets Wed 19:30 (in 4h36m)
7d  20% used  allowed  0.5%/h`
)

// guardEnv gives one test its own stamp, off switch directory and config, and
// short pause bounds. The pause itself does not sleep.
func guardEnv(t *testing.T) (dir string) {
	t.Helper()
	dir = t.TempDir()
	t.Setenv("CLUSAGE_GUARD_STATE", filepath.Join(dir, "stamp"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, "claude"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("CLUSAGE_GUARD_FIXTURE", filepath.Join(dir, "fx"))
	t.Setenv("CLUSAGE_GUARD_POLL", "1")
	t.Setenv("CLUSAGE_GUARD_MAXWAIT", "2")
	for _, k := range []string{"CLUSAGE_GUARD_DISABLE", "CLUSAGE_RESUME_DISABLE", "CLUSAGE_GUARD_5H",
		"CLUSAGE_GUARD_7D", "CLUSAGE_GUARD_INTERVAL", "CLUSAGE_GUARD_INTERVAL_MIN",
		"CLUSAGE_GUARD_ALLOW_OVERAGE", "CLUSAGE_GUARD_ALLOW_TOOLS", "CLUSAGE_GUARD_HANDOFF"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	old := guardSleep
	guardSleep = func(time.Duration) {}
	t.Cleanup(func() { guardSleep = old })
	return dir
}

// cwdFor returns a function that puts a session directory under dir in place
// of CWD, JSON escaped the way both the payload and the deny carry it, so the
// cases read the same on Windows.
func cwdFor(dir string) func(string) string {
	work := filepath.Join(dir, "work")
	esc := strings.Trim(encodeJSON(work+string(filepath.Separator)), "\"\n")
	return func(s string) string {
		s = strings.ReplaceAll(s, "CWD/", esc)
		return strings.ReplaceAll(s, "CWD", strings.Trim(encodeJSON(work), "\"\n"))
	}
}

// runGuard writes the fixture, clears the stamp, and runs one hook event.
func runGuard(t *testing.T, fixture, payload string) (out, errOut string) {
	t.Helper()
	fx := os.Getenv("CLUSAGE_GUARD_FIXTURE")
	if err := os.WriteFile(fx, []byte(fixture+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Remove(os.Getenv("CLUSAGE_GUARD_STATE"))
	return hookOnce(payload)
}

func hookOnce(payload string) (string, string) {
	var out, errOut bytes.Buffer
	hookRun(strings.NewReader(payload), &out, &errOut)
	return out.String(), errOut.String()
}

func TestGuardDecisions(t *testing.T) {
	cases := []struct {
		name, fixture, payload string
		want                   string // "" for allow, else a substring of the deny
	}{
		{"low", fxLow, "", ""},
		{"high 5h", fxHigh5, "", "5h limit is at 94% and did not drop"},
		{"just under soft", "5h  89% used  allowed_warning\n7d  20% used  allowed", "", ""},
		{"high 7d", fxHigh7, "", "7d limit is at 96%"},
		{"7d-opus", "5h  10% used  allowed\n7d  40% used  allowed\n7d-opus  97% used  allowed_warning", "", "7d limit is at 97%"},
		{"edge", "5h  90% used  allowed\n7d  95% used  allowed", "", "7d limit is at 95%"},
		{"fraction 7d", "5h  33% used  allowed\n7d  95.5% used  allowed_warning", "", "7d limit is at 95.5%"},
		{"fraction 5h", "5h  90.5% used  allowed_warning\n7d  20% used  allowed", "", "5h limit is at 90.5%"},
		{"under hard", "5h  33% used  allowed\n7d  94.5% used  allowed_warning", "", ""},
		{"soft edge", "5h  90% used  allowed\n7d  20% used  allowed", "", "5h limit is at 90%"},
		{"reset clock", fxHigh5, "", "It resets Wed 19:30 (in 4h36m). Stop all other work now"},
		{"no reset", fxHigh7, "", "reported no reset time"},
		{"exhausted", fxBurned, "", "5h window is exhausted (status rejected)"},
		{"rate is not a status", "5h  61% used              14.2%/h  resets Wed 19:30 (in 4h)\n7d  20% used  allowed", "", ""},

		{"empty", "", "", "no usable rate limit window"},
		{"no headers", "clusage: no usable anthropic-ratelimit-unified-* headers on the response", "", "no usable rate limit window"},
		{"error joined", "clusage: the API rejected the OAuth token (401 Unauthorized). Run claude to log in again.\nusage: GET \"https://api.anthropic.com/api/oauth/usage\": 401 Unauthorized", "",
			"The error from clusage: the API rejected the OAuth token (401 Unauthorized). Run claude to log in again. usage: GET"},
		{"no 7d", "5h  61% used  allowed  resets Wed 19:30 (in 4h)", "", "no usable rate limit window"},
		{"no 5h", "7d  20% used  allowed", "", "no usable rate limit window"},
		{"nodata off switch", "", "", "clusage guard off"},
		{"nodata force", "", "", "clusage usage -force"},

		{"schedule passes", fxHigh7, `{"hook_event_name":"PreToolUse","tool_name":"ScheduleWakeup","tool_input":{}}`, ""},
		{"bash denied", fxHigh7, `{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{}}`, "7d limit is at 96%"},
		{"question passes", fxHigh7, `{"hook_event_name":"PreToolUse","tool_name":"AskUserQuestion","tool_input":{}}`, ""},

		{"trend rate", fxHigh5Rate, "", "rising at 12.0%/h"},
		{"trend fill", fxHigh5Rate, "", "fills in about 30m"},
		{"ask first", fxHigh5, "", "Do not schedule a resume before the user answers"},
		{"multiple choice", fxHigh5, "", "multiple choice question"},
		{"overage option", fxHigh5, "", "keep working now and pay overage"},
		{"timer only on wait", fxHigh5, "", "only if the user picks"},
		{"no reset question", fxHigh7, "", "ask whether to write the current state to a handoff file and stop, keep working and pay overage, or stop here"},
		{"trend rate", fxHigh5Rate, "", "rising at 12.0%/h"},
		{"trend fill", fxHigh5Rate, "", "fills in about 30m"},
		{"ask first", fxHigh5, "", "Do not schedule a resume before the user answers"},
		{"multiple choice", fxHigh5, "", "multiple choice question"},
		{"overage option", fxHigh5, "", "keep working now and pay overage"},
		{"timer only on wait", fxHigh5, "", "only if the user picks"},
		{"no reset question", fxHigh7, "", "ask whether to write the current state to a handoff file and stop, keep working and pay overage, or stop here"},
		{"handoff option", fxHigh5, "", "write the current state to a handoff file so a fresh session can resume from it and stop"},
		{"handoff steps", fxHigh5, `{"tool_name":"Bash","cwd":"CWD"}`, "the guard lets Read, Write and Edit through for CWD/HANDOFF.md"},
		{"handoff resume", fxHigh5, `{"tool_name":"Bash","cwd":"CWD"}`, "read CWD/HANDOFF.md and continue from its next steps"},
		{"handoff 7d", fxHigh7, "", "handoff file"},
		{"handoff other file", fxHigh5, `{"tool_name":"Write","cwd":"CWD","tool_input":{"file_path":"CWD/main.go"}}`, "5h limit is at 94%"},
		{"handoff pass", fxHigh5, `{"tool_name":"Write","cwd":"CWD","tool_input":{"file_path":"CWD/HANDOFF.md"}}`, ""},
		{"handoff read", fxHigh7, `{"tool_name":"Read","cwd":"CWD","tool_input":{"file_path":"CWD/HANDOFF.md"}}`, ""},
		{"handoff relative", fxHigh5, `{"tool_name":"Edit","cwd":"CWD","tool_input":{"file_path":"HANDOFF.md"}}`, ""},
		{"handoff bash", fxHigh5, `{"tool_name":"Bash","cwd":"CWD","tool_input":{"file_path":"CWD/HANDOFF.md"}}`, "5h limit is at 94%"},
		{"handoff spent", fxBurned, `{"tool_name":"Write","cwd":"CWD","tool_input":{"file_path":"CWD/HANDOFF.md"}}`, "5h window is exhausted (status rejected)"},
		{"handoff nodata", "", `{"tool_name":"Write","cwd":"CWD","tool_input":{"file_path":"CWD/HANDOFF.md"}}`, "no usable rate limit window"},
		{"no reset decide", fxHigh7, "", "Do not decide it yourself"},
		{"allow list 5h", fxHigh5, "", "still allows these tools"},
		{"allow list names", fxHigh5, "", "AskUserQuestion"},
		{"allow list 7d", fxHigh7, "", "still allows these tools"},
		{"allow list spent", fxBurned, "", "still allows these tools"},
		{"lever 5h", fxHigh5, "", "clusage guard off"},
		{"lever 7d", fxHigh7, "", "clusage guard on"},
		{"lever spent", fxBurned, "", "clusage guard off"},
		{"legs", fxHigh5, "", "legs of 55 minutes or less"},
		{"leg message", fxHigh5, "", "Put the leg number, the total, and the reset time into the message"},
		{"leg read", fxHigh5, "", "On waking, read the leg number from that message"},
		{"leg next", fxHigh5, "", "schedule the next leg and do nothing else. Then end the turn"},
		{"no clock", fxHigh5, "", "Never call a tool to check the clock"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cwd := cwdFor(guardEnv(t))
			out, _ := runGuard(t, c.fixture, cwd(c.payload))
			c.want = cwd(c.want)
			if c.want == "" {
				if out != "" {
					t.Fatalf("want allow, got %s", out)
				}
				return
			}
			if !strings.Contains(out, `"permissionDecision":"deny"`) || !strings.Contains(out, c.want) {
				t.Fatalf("want a deny with %q, got %s", c.want, out)
			}
		})
	}
}

func TestGuardDenyShape(t *testing.T) {
	guardEnv(t)
	out, _ := runGuard(t, fxHigh7, "")
	want := `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"clusage guard rail: the 7d limit is at 96% (hard cut at 95%). The 7d window`
	if !strings.HasPrefix(out, want) || !strings.HasSuffix(out, "\"}}\n") {
		t.Fatalf("deny shape changed:\n%s", out)
	}
}

func TestGuardTextAbsent(t *testing.T) {
	cases := []struct{ name, fixture, not string }{
		{"no rate, no trend", fxHigh5, "rising at"},
		{"no reset, no legs", fxHigh7, "read the leg number from that message"},
		{"spent, no handoff", fxBurned, "handoff"},
	}
	for _, c := range cases {
		guardEnv(t)
		if out, _ := runGuard(t, c.fixture, ""); strings.Contains(out, c.not) {
			t.Errorf("%s: got %s", c.name, out)
		}
	}
}

func TestGuardOverageOptIn(t *testing.T) {
	guardEnv(t)
	t.Setenv("CLUSAGE_GUARD_ALLOW_OVERAGE", "1")
	if out, _ := runGuard(t, fxBurned, ""); !strings.Contains(out, "5h limit is at 100% and did not drop") {
		t.Fatalf("overage opt-in, got %s", out)
	}
}

// On overage the handoff drops out of the deny, and its write is denied,
// because overage would pay for it.
func TestGuardHandoffNeverOnOverage(t *testing.T) {
	cwd := cwdFor(guardEnv(t))
	t.Setenv("CLUSAGE_GUARD_ALLOW_OVERAGE", "1")
	if out, _ := runGuard(t, fxBurned, ""); !strings.Contains(out, "did not drop") || strings.Contains(out, "handoff") {
		t.Fatalf("an exhausted window offered the handoff: %s", out)
	}
	out, _ := runGuard(t, fxBurned, cwd(`{"tool_name":"Write","cwd":"CWD","tool_input":{"file_path":"CWD/HANDOFF.md"}}`))
	if !strings.Contains(out, "overage would pay for the handoff file") {
		t.Fatalf("the handoff write passed on overage: %s", out)
	}
}

// The handoff file follows the setting, and "off" drops the option.
func TestGuardHandoffSetting(t *testing.T) {
	cwd := cwdFor(guardEnv(t))
	t.Setenv("CLUSAGE_GUARD_HANDOFF", "next.md")
	if out, _ := runGuard(t, fxHigh5, cwd(`{"tool_name":"Bash","cwd":"CWD"}`)); !strings.Contains(out, cwd("CWD/next.md")) {
		t.Fatalf("the setting did not name the file: %s", out)
	}
	if out, _ := runGuard(t, fxHigh5, cwd(`{"tool_name":"Write","cwd":"CWD","tool_input":{"file_path":"CWD/next.md"}}`)); out != "" {
		t.Fatalf("the configured file was denied: %s", out)
	}
	t.Setenv("CLUSAGE_GUARD_HANDOFF", "off")
	if out, _ := runGuard(t, fxHigh5, ""); strings.Contains(out, "handoff") || !strings.Contains(out, "Offer three options") {
		t.Fatalf("off still offered the handoff: %s", out)
	}
	if out, _ := runGuard(t, fxHigh5, cwd(`{"tool_name":"Write","cwd":"CWD","tool_input":{"file_path":"CWD/HANDOFF.md"}}`)); out == "" {
		t.Fatal("off still let the handoff write through")
	}
}

func TestGuardOffSwitch(t *testing.T) {
	dir := guardEnv(t)
	if err := os.MkdirAll(filepath.Join(dir, "claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := setGuardOff(offPath(), true); err != nil {
		t.Fatal(err)
	}
	if out, _ := runGuard(t, fxHigh7, ""); out != "" {
		t.Fatalf("off switch, got %s", out)
	}
	t.Setenv("CLUSAGE_GUARD_DISABLE", "1")
	setGuardOff(offPath(), false)
	if out, _ := runGuard(t, fxHigh7, ""); out != "" {
		t.Fatalf("disable variable, got %s", out)
	}
}

// A pause that sees the 5h window drop lets the call through and says so.
func TestGuardResumesAfterPause(t *testing.T) {
	guardEnv(t)
	t.Setenv("CLUSAGE_GUARD_MAXWAIT", "10")
	guardSleep = func(time.Duration) {
		os.WriteFile(os.Getenv("CLUSAGE_GUARD_FIXTURE"), []byte(fxLow), 0o600)
	}
	out, errOut := runGuard(t, fxHigh5, "")
	if out != "" || !strings.Contains(errOut, "5h usage back down to 33%, work resumed after 1s.") {
		t.Fatalf("out=%q err=%q", out, errOut)
	}
}

// POLL=0 never advances the wait, so the pause would never end. It falls back.
func TestGuardPollZeroStillDenies(t *testing.T) {
	guardEnv(t)
	t.Setenv("CLUSAGE_GUARD_POLL", "0")
	n := 0
	guardSleep = func(d time.Duration) {
		if n++; n > 100 || d <= 0 {
			panic("the pause does not advance")
		}
	}
	if out, _ := runGuard(t, fxHigh5, ""); !strings.Contains(out, "did not drop") {
		t.Fatalf("got %s", out)
	}
}

// A panic anywhere in the check still denies.
func TestGuardPanicDenies(t *testing.T) {
	guardEnv(t)
	old := guardRead
	guardRead = func(int) ([]usageRow, string) { panic("boom") }
	t.Cleanup(func() { guardRead = old })
	if out, _ := runGuard(t, fxLow, ""); !strings.Contains(out, "no usable rate limit window") {
		t.Fatalf("got %s", out)
	}
}

func TestIntervalFor(t *testing.T) {
	g := defaultConfig.Guard
	cases := []struct {
		p5, p7 float64
		want   int
	}{
		{0, 0, 300}, {10, 5, 297}, {50, 20, 217}, {70, 30, 137}, {89, 20, 36},
		{90, 20, 30}, {100, 20, 30}, {20, 90, 58}, {20, 95, 30}, {-1, -1, 300},
		{awkNum("abc"), awkNum("abc"), 300},
	}
	for _, c := range cases {
		if got := intervalFor(c.p5, c.p7, g); got != c.want {
			t.Errorf("intervalFor(%v, %v) = %d, want %d", c.p5, c.p7, got, c.want)
		}
	}
	bounds := []struct {
		hi, lo       int
		p5, p7, want int
	}{
		{60, 900, 10, 5, 60},    // a floor above the ceiling is a typo
		{0, 30, 10, 5, 0},       // a zero ceiling checks every call
		{360, 360, 88, 20, 360}, // equal bounds give a fixed interval
	}
	for _, b := range bounds {
		g := defaultConfig.Guard
		g.Interval, g.IntervalMin = b.hi, b.lo
		if got := intervalFor(float64(b.p5), float64(b.p7), g); got != b.want {
			t.Errorf("bounds %d/%d: got %d, want %d", b.hi, b.lo, got, b.want)
		}
	}
}

// A bad bound falls back instead of breaking the guard.
func TestGuardBadBoundsFallBack(t *testing.T) {
	guardEnv(t)
	for _, bad := range []string{"abc", "", "-5", "3.5"} {
		t.Setenv("CLUSAGE_GUARD_INTERVAL", bad)
		if got := intervalFor(10, 5, guardSettings()); got != 297 {
			t.Errorf("ceiling %q: got %d, want 297", bad, got)
		}
	}
	os.Unsetenv("CLUSAGE_GUARD_INTERVAL")
	t.Setenv("CLUSAGE_GUARD_INTERVAL_MIN", "abc")
	if got := intervalFor(90, 20, guardSettings()); got != 30 {
		t.Errorf("floor abc: got %d, want 30", got)
	}
	for _, bad := range []string{"CLUSAGE_GUARD_MAXWAIT", "CLUSAGE_GUARD_POLL"} {
		t.Setenv(bad, "abc")
	}
	g := guardSettings()
	if g.MaxWait != 45 || g.Poll != 15 {
		t.Errorf("bad pause bounds: maxwait %d poll %d", g.MaxWait, g.Poll)
	}
}

func TestProject(t *testing.T) {
	cases := []struct {
		p, r, c float64
		want    string
	}{
		{30, 30, 90, "1800"}, {89, 30, 90, "30"},
		{30, 0, 90, ""}, {30, -5, 90, ""}, {95, 30, 90, ""},
	}
	for _, c := range cases {
		got := ""
		if n, ok := project(c.p, c.r, c.c); ok {
			got = fmt.Sprint(n)
		}
		if got != c.want {
			t.Errorf("project(%v, %v, %v) = %q, want %q", c.p, c.r, c.c, got, c.want)
		}
	}
}

// gate checks whether a stamp holds the next check back. fxHigh7 denies every
// call, so a probe shows as output and a skip as silence.
func TestGuardStampGate(t *testing.T) {
	age := func(s int64) string { return fmt.Sprint(time.Now().Unix() - s) }
	cases := []struct {
		stamp string // "-" for no stamp
		probe bool
		label string
		env   [2]string
	}{
		{age(100) + " 10 5", false, "low usage, recent check", [2]string{}},
		{age(100) + " 90 20", true, "high usage, recent check", [2]string{}},
		{age(100) + " 20 92", true, "high 7d, recent check", [2]string{}},
		{age(400) + " 10 5", true, "low usage, stale check", [2]string{}},
		{age(100), false, "old stamp format", [2]string{}},
		{age(400), true, "old stamp format, stale", [2]string{}},
		{"garbage", true, "unreadable stamp", [2]string{}},
		{"", true, "empty stamp", [2]string{}},
		{"-", true, "no stamp", [2]string{}},
		{age(100) + " 10 5 -1 -1", false, "low usage, no rate", [2]string{}},
		{age(100) + " 10 5 60 0", false, "low usage, slow climb", [2]string{}},
		{age(100) + " 10 5 3000 0", true, "low usage, violent climb", [2]string{}},
		{age(100) + " 5 10 0 3000", true, "high 7d rate drives the gate", [2]string{}},
		{age(100) + " 88 5 -1 -1", true, "near the cut, no rate", [2]string{}},
		{age(0) + " 10 5 -1 -1", true, "zero interval checks every call", [2]string{"CLUSAGE_GUARD_INTERVAL", "0"}},
		{age(100) + " 33 96", true, "floor above ceiling", [2]string{"CLUSAGE_GUARD_INTERVAL_MIN", "900"}},
		{age(400) + " 10 5 60 0", true, "non-numeric floor", [2]string{"CLUSAGE_GUARD_INTERVAL_MIN", "abc"}},
		{age(27) + " 10 5 3000 0", false, "floor 030 reads as 30", [2]string{"CLUSAGE_GUARD_INTERVAL_MIN", "030"}},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			guardEnv(t)
			if c.env[0] != "" {
				t.Setenv(c.env[0], c.env[1])
			}
			if c.label == "floor above ceiling" {
				t.Setenv("CLUSAGE_GUARD_INTERVAL", "60")
			}
			os.WriteFile(os.Getenv("CLUSAGE_GUARD_FIXTURE"), []byte(fxHigh7), 0o600)
			if c.stamp != "-" {
				os.WriteFile(os.Getenv("CLUSAGE_GUARD_STATE"), []byte(c.stamp+"\n"), 0o600)
			}
			out, _ := hookOnce("")
			if c.probe != (out != "") {
				t.Fatalf("want probe=%v, got %q", c.probe, out)
			}
		})
	}
}

func TestGuardStampRecordsTheCheck(t *testing.T) {
	for _, c := range []struct{ fixture, want string }{
		{fxLow, "33 20 -1 -1"},
		{fxLowRate, "33 20 14.2 0.9"},
	} {
		guardEnv(t)
		runGuard(t, c.fixture, "")
		raw, _ := os.ReadFile(os.Getenv("CLUSAGE_GUARD_STATE"))
		f := strings.Fields(string(raw))
		if len(f) != 5 || strings.Join(f[1:], " ") != c.want {
			t.Errorf("stamp = %q, want <time> %s", raw, c.want)
		}
	}
}

// The reading cache tracks the wait, so a fast poll never reads a stale probe.
func TestGuardCacheMinutes(t *testing.T) {
	var got []int
	fixture := ""
	old := guardRead
	guardRead = func(m int) ([]usageRow, string) {
		got = append(got, m)
		return parseRows(fixture), ""
	}
	t.Cleanup(func() { guardRead = old })
	age := fmt.Sprint(time.Now().Unix() - 400)
	for _, c := range []struct {
		fixture, stamp string
		want           []int
	}{
		{fxLow, age + " 10 5", []int{4}},  // low usage waits 297s
		{fxLow, age + " 89 20", []int{0}}, // near the cut the probe is live
		{fxHigh5, "", []int{5, 0, 0}},     // every poll in a pause is live
	} {
		guardEnv(t)
		got, fixture = nil, c.fixture
		os.WriteFile(os.Getenv("CLUSAGE_GUARD_STATE"), []byte(c.stamp), 0o600)
		hookOnce("")
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("stamp %q: cache minutes %v, want %v", c.stamp, got, c.want)
		}
	}
}

// The config file sets the cuts and the allow list, and the environment wins.
func TestGuardReadsTheConfigFile(t *testing.T) {
	guardEnv(t)
	cfgDir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "clusage")
	os.MkdirAll(cfgDir, 0o700)
	os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(`{"source":"usage","guard":{
		"soft_5h_percent":50,"interval_seconds":0,"interval_min_seconds":0,
		"poll_seconds":1,"max_wait_seconds":2,"allow_tools":["Solo"]}}`), 0o600)
	fx := "5h  60% used  allowed  resets Wed 19:30 (in 4h36m)\n7d  20% used  allowed"
	old := guardRead
	guardRead = func(int) ([]usageRow, string) { return parseRows(fx), "" }
	t.Cleanup(func() { guardRead = old })
	os.Unsetenv("CLUSAGE_GUARD_FIXTURE")
	os.Unsetenv("CLUSAGE_GUARD_POLL")
	os.Unsetenv("CLUSAGE_GUARD_MAXWAIT")

	if out, _ := hookOnce(""); !strings.Contains(out, "soft limit 50%") || !strings.Contains(out, "did not drop in 2s") {
		t.Errorf("config cuts not applied, got %s", out)
	}
	if out, _ := hookOnce(`{"tool_name":"Solo"}`); out != "" {
		t.Errorf("config allow list not applied, got %s", out)
	}
	t.Setenv("CLUSAGE_GUARD_5H", "99")
	if out, _ := hookOnce(""); out != "" {
		t.Errorf("env must override the config soft cut, got %s", out)
	}
}

func TestResumeReport(t *testing.T) {
	stale := `{"hook_event_name":"SessionStart","source":"resume",
"seconds_since_last_response":5400,"context_tokens":182340,
"prompt_cache_likely_expired":true,"estimated_cache_write_usd":1.1396}`
	cases := []struct{ payload, want string }{
		{stale, `"systemMessage"`},
		{stale, `"additionalContext"`},
		{stale, "expired after 90m idle"},
		{stale, "re-sends 182k tokens, about $1.14"},
		{strings.Replace(stale, "true", "false", 1), ""},
		{`{"hook_event_name":"SessionStart","source":"resume","prompt_cache_likely_expired":true}`, ""},
		{`{"hook_event_name":"SessionStart","source":"startup"}`, ""},
		// a tool call still reaches the guard rail, payload and all
		{`{"hook_event_name":"PreToolUse","tool_name":"Bash"}`, "7d limit is at 96%"},
	}
	for _, c := range cases {
		guardEnv(t)
		out, _ := runGuard(t, fxHigh7, c.payload)
		if (c.want == "" && out != "") || !strings.Contains(out, c.want) {
			t.Errorf("payload %.60s: want %q, got %s", c.payload, c.want, out)
		}
	}
	guardEnv(t)
	t.Setenv("CLUSAGE_RESUME_DISABLE", "1")
	if out, _ := runGuard(t, fxHigh7, stale); out != "" {
		t.Errorf("disable, got %s", out)
	}
}

// The in-process rows match what clusage usage prints for the same reading.
func TestRowsFromReading(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	u := usageRead{now: now, r: Reading{FetchedAt: now, Headers: map[string]string{
		"anthropic-ratelimit-unified-5h-utilization": "0.945",
		"anthropic-ratelimit-unified-5h-status":      "allowed_warning",
		"anthropic-ratelimit-unified-5h-reset":       fmt.Sprint(now.Add(90 * time.Minute).Unix()),
		"anthropic-ratelimit-unified-7d-utilization": "0.2",
		"anthropic-ratelimit-unified-7d-status":      "allowed",
		"anthropic-ratelimit-unified-overage-status": "allowed",
	}}}
	rows := rowsFromReading(u)
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	five, seven := rows[0], rows[1]
	if five.name != "5h" || five.pct != 94 || five.status != "allowed_warning" ||
		!strings.HasPrefix(five.reset, "resets ") || five.rate != "" {
		t.Errorf("5h row = %+v", five)
	}
	if seven.name != "7d" || seven.pct != 20 || seven.reset != "" {
		t.Errorf("7d row = %+v", seven)
	}
}
