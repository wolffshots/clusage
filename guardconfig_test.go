package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestGuardNormalize checks the bounds the hook script enforces. A value the
// script would drop has to be dropped here too, or the Config tab reports a
// number the guard never applies.
func TestGuardNormalize(t *testing.T) {
	d := defaultConfig.Guard

	// Zero is legal for three of them: no ceiling, no floor, no wait.
	g := Guard{Interval: 0, IntervalMin: 0, MaxWait: 0, Poll: 1,
		Soft5h: 0, Hard7d: 0, AllowTools: []string{"A"}}
	g.normalize()
	if g.Interval != 0 || g.IntervalMin != 0 || g.MaxWait != 0 {
		t.Errorf("a legal zero was replaced: %+v", g)
	}
	if g.Soft5h != 0 || g.Hard7d != 0 {
		t.Errorf("a zero cut was replaced: %+v", g)
	}

	// Everything out of range falls back, and an empty allow list reads as
	// unset because the hook script cannot express one either.
	g = Guard{Soft5h: 101, Hard7d: -1, Interval: -1, IntervalMin: -5,
		Poll: 0, MaxWait: -2, AllowTools: []string{}}
	g.normalize()
	if !reflect.DeepEqual(g, d) {
		t.Errorf("bad values did not fall back to the defaults:\ngot  %+v\nwant %+v", g, d)
	}
}

// TestEffectiveGuardEnv checks the precedence the hook script uses: a usable
// environment variable wins, and an unusable one leaves the file alone.
func TestEffectiveGuardEnv(t *testing.T) {
	file := Guard{Soft5h: 95, Hard7d: 96, Interval: 60, IntervalMin: 10,
		Poll: 5, MaxWait: 20, AllowTools: []string{"Solo"}}

	got, over := effectiveGuard(file)
	if got.Soft5h != 95 || len(over) != 0 {
		t.Errorf("no environment must leave the file untouched: %+v %v", got, over)
	}

	t.Setenv(envSoft5h, "80")
	t.Setenv(envAllowTools, "One Two")
	t.Setenv(envPoll, "not-a-number")
	got, over = effectiveGuard(file)
	if got.Soft5h != 80 {
		t.Errorf("the environment must win, got %d", got.Soft5h)
	}
	if strings.Join(got.AllowTools, ",") != "One,Two" {
		t.Errorf("the allow list did not come from the environment: %v", got.AllowTools)
	}
	if got.Poll != 5 {
		t.Errorf("a bad override must fall through to the file, got %d", got.Poll)
	}
	if strings.Join(over, ",") != "5H,ALLOW_TOOLS" {
		t.Errorf("the overridden names are wrong: %v", over)
	}
}

// TestWriteGuardConfig checks the lines the hook script parses. The script
// takes a known key with a plain value and drops everything else, so a change
// of key name here silently disables that setting.
func TestWriteGuardConfig(t *testing.T) {
	var b strings.Builder
	if err := writeGuardConfig(&b, Guard{Soft5h: 95, Hard7d: 97, Interval: 300,
		IntervalMin: 30, Poll: 15, MaxWait: 45, AllowOverage: true,
		AllowTools: []string{"One", "Two"}, Handoff: "HANDOFF.md"}); err != nil {
		t.Fatal(err)
	}
	want := "soft_5h=95\nhard_7d=97\ninterval=300\ninterval_min=30\n" +
		"poll=15\nmaxwait=45\nallow_overage=1\nallow_tools=One Two\nhandoff_file=HANDOFF.md\n"
	if b.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", b.String(), want)
	}
}

// TestDefaultConfigFileIsComplete checks that the file written on first run
// names every field. A field left out of the file is a setting the user cannot
// discover without reading the source.
func TestDefaultConfigFileIsComplete(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg, path, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"model", "threshold_minutes", "fetch_cron",
		"history_hours", "guard"} {
		if _, ok := got[key]; !ok {
			t.Errorf("the default file has no %q", key)
		}
	}
	guard, _ := got["guard"].(map[string]any)
	for _, key := range []string{"soft_5h_percent", "hard_7d_percent",
		"interval_seconds", "interval_min_seconds", "poll_seconds",
		"max_wait_seconds", "allow_overage", "allow_tools", "handoff_file"} {
		if _, ok := guard[key]; !ok {
			t.Errorf("the default guard section has no %q", key)
		}
	}
	if !reflect.DeepEqual(cfg.Guard, defaultConfig.Guard) {
		t.Errorf("the defaults were not loaded back: %+v", cfg.Guard)
	}
}

// TestLoadConfigPartialGuard checks that a file naming one guard field keeps
// the defaults for the rest.
func TestLoadConfigPartialGuard(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "clusage"), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"model":"m","guard":{"soft_5h_percent":95}}`
	if err := os.WriteFile(filepath.Join(dir, "clusage", "config.json"),
		[]byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Guard.Soft5h != 95 {
		t.Errorf("the file value was lost, got %d", cfg.Guard.Soft5h)
	}
	if cfg.Guard.Hard7d != defaultConfig.Guard.Hard7d {
		t.Errorf("an absent field did not keep its default, got %d", cfg.Guard.Hard7d)
	}
}

// TestReadGuardStatus checks what the Config tab reports about the hook.
func TestReadGuardStatus(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)

	if st := readGuardStatus(); st.Registered || st.Off {
		t.Errorf("an empty directory must report nothing: %+v", st)
	}

	settings := `{"hooks":{"PreToolUse":[{"matcher":"*","hooks":[` +
		`{"type":"command","command":"bash '/x/hooks/clusage-guard.sh'"}]}]}}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"),
		[]byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	if st := readGuardStatus(); !st.Registered {
		t.Error("a registered hook was not seen")
	}

	if err := os.WriteFile(filepath.Join(dir, "clusage-guard.off"),
		nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLUSAGE_GUARD_DISABLE", "1")
	st := readGuardStatus()
	if !st.Off || !st.Disabled {
		t.Errorf("the off switch and the disable variable were not seen: %+v", st)
	}
}

// TestGuardOffKey checks that o on the Now tab creates the off switch file,
// that a second press removes it, and that the tab reports what is on disk.
func TestGuardOffKey(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	off := filepath.Join(dir, "clusage-guard.off")
	m := testModel("")
	press := func() {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
		m = next.(model)
	}

	press()
	if _, err := os.Stat(off); err != nil || !m.guard.Off {
		t.Fatalf("o did not create the off switch: err=%v guard=%+v", err, m.guard)
	}
	if !strings.Contains(m.nowView(40), "GUARD OFF") {
		t.Error("the Now tab does not say the guard is off")
	}

	press()
	if _, err := os.Stat(off); !os.IsNotExist(err) || m.guard.Off {
		t.Fatalf("o did not remove the off switch: err=%v guard=%+v", err, m.guard)
	}
	if !strings.Contains(m.nowView(40), "GUARD ON") {
		t.Error("the Now tab does not say the guard is on")
	}

	// Another tab does not show the switch, so it must not flip the file.
	m.active = viewConfig
	press()
	if _, err := os.Stat(off); !os.IsNotExist(err) {
		t.Error("o flipped the off switch outside the Now tab")
	}
}

// TestTilde checks the path shortening the Config tab uses. A screenshot of
// that tab must not carry the user name.
func TestTilde(t *testing.T) {
	// os.UserHomeDir reads USERPROFILE on Windows and HOME elsewhere.
	home := filepath.FromSlash("/Users/someone")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for path, want := range map[string]string{
		"/Users/someone/.config/clusage/config.json": "~/.config/clusage/config.json",
		"/Users/someone":        "/Users/someone",
		"/Users/someone-else/x": "/Users/someone-else/x",
		"/etc/clusage.json":     "/etc/clusage.json",
	} {
		path, want = filepath.FromSlash(path), filepath.FromSlash(want)
		if got := tilde(path); got != want {
			t.Errorf("tilde(%q) = %q, want %q", path, got, want)
		}
	}
}
