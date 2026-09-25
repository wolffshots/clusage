package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestHookRejectsUnknownAction(t *testing.T) {
	if err := hook([]string{"enable"}); err == nil {
		t.Fatal("hook(enable) = nil, want an error")
	}
}

type testHook struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
	Timeout int      `json:"timeout"`
}

type testEntry struct {
	Matcher string     `json:"matcher"`
	Hooks   []testHook `json:"hooks"`
}

func readTestSettings(t *testing.T, path string) (map[string][]testEntry, string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Hooks map[string][]testEntry `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return s.Hooks, string(raw)
}

func TestHookRegistrationRoundTrip(t *testing.T) {
	guardEnv(t)
	dir := t.TempDir()
	os.Unsetenv("CLUSAGE_GUARD_MAXWAIT")
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	path := filepath.Join(dir, "settings.json")
	os.WriteFile(path, []byte(`{"model":"opus","hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"x"}]}]},"z":"<&>"}`+"\n"), 0o600)

	if err := manageHook("status", dir); err == nil {
		t.Error("status before install: want an error")
	}
	for range 2 { // install is idempotent
		if err := manageHook("install", dir); err != nil {
			t.Fatal(err)
		}
	}
	hooks, raw := readTestSettings(t, path)
	if !strings.HasPrefix(raw, "{\n  \"model\"") || !strings.Contains(raw, `"z": "<&>"`) {
		t.Errorf("key order or unknown keys lost:\n%s", raw)
	}
	if len(hooks["PreToolUse"]) != 1 || hooks["PreToolUse"][0].Matcher != "*" ||
		hooks["PreToolUse"][0].Hooks[0].Timeout != 60 {
		t.Errorf("PreToolUse = %+v", hooks["PreToolUse"])
	}
	ss := hooks["SessionStart"]
	if len(ss) != 2 || ss[0].Hooks[0].Command != "x" || ss[1].Matcher != "resume|fork" {
		t.Errorf("SessionStart = %+v", ss)
	}
	h := hooks["PreToolUse"][0].Hooks[0]
	if line := strings.TrimSpace(h.Command + " " + strings.Join(h.Args, " ")); !strings.HasSuffix(line, "hook run") {
		t.Errorf("command = %q", line)
	}
	if !readGuardStatus().Registered {
		t.Error("the TUI does not see the registration")
	}

	if err := manageHook("uninstall", dir); err != nil {
		t.Fatal(err)
	}
	hooks, _ = readTestSettings(t, path)
	if _, ok := hooks["PreToolUse"]; ok || len(hooks["SessionStart"]) != 1 || hooks["SessionStart"][0].Hooks[0].Command != "x" {
		t.Errorf("uninstall left residue: %+v", hooks)
	}
}

// An install over the script-based version rewrites its entry, adds the new
// event, and removes the old link, but never a real file.
func TestHookInstallMigratesTheScript(t *testing.T) {
	guardEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	os.WriteFile(path, []byte(`{"model":"opus","hooks":{"PreToolUse":[{"matcher":"*","hooks":[{"type":"command","command":"bash '/x/hooks/clusage-guard.sh'","timeout":60}]}]}}`), 0o600)
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	if st := readGuardStatus(); !st.Registered || !st.Legacy {
		t.Errorf("legacy entry not seen: %+v", st)
	}

	os.MkdirAll(filepath.Join(dir, "hooks"), 0o755)
	link := filepath.Join(dir, "hooks", hookScriptName)
	linked := os.Symlink(filepath.Join(dir, "elsewhere.sh"), link) == nil
	if !linked && runtime.GOOS != "windows" {
		t.Fatal("symlink failed")
	}

	if err := manageHook("install", dir); err != nil {
		t.Fatal(err)
	}
	hooks, raw := readTestSettings(t, path)
	if len(hooks["PreToolUse"]) != 1 || len(hooks["SessionStart"]) != 1 || strings.Contains(raw, "clusage-guard") {
		t.Errorf("migration: %s", raw)
	}
	if st := readGuardStatus(); !st.Registered || st.Legacy {
		t.Errorf("after migration: %+v", st)
	}
	if _, err := os.Lstat(link); linked && err == nil {
		t.Error("the old link survived")
	}

	os.WriteFile(link, []byte("mine"), 0o644)
	manageHook("uninstall", dir)
	if b, _ := os.ReadFile(link); string(b) != "mine" {
		t.Error("a real file at the link path was removed")
	}
}

// An entry that holds another tool's hook beside the guard keeps that hook and
// its matcher through an install and an uninstall.
func TestHookLeavesASharedEntryAlone(t *testing.T) {
	guardEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	os.WriteFile(path, []byte(`{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[`+
		`{"type":"command","command":"clusage hook run"},{"type":"command","command":"other-tool"}]}]}}`), 0o600)
	other := func(step string) {
		t.Helper()
		hooks, raw := readTestSettings(t, path)
		e := hooks["PreToolUse"][0]
		if e.Matcher != "Bash" || len(e.Hooks) != 1 || e.Hooks[0].Command != "other-tool" {
			t.Fatalf("%s changed the other tool's entry:\n%s", step, raw)
		}
	}
	if err := manageHook("install", dir); err != nil {
		t.Fatal(err)
	}
	other("install")
	if hooks, raw := readTestSettings(t, path); len(hooks["PreToolUse"]) != 2 || hooks["PreToolUse"][1].Matcher != "*" {
		t.Fatalf("install did not give the guard its own entry:\n%s", raw)
	}
	if err := manageHook("uninstall", dir); err != nil {
		t.Fatal(err)
	}
	other("uninstall")
	if hooks, raw := readTestSettings(t, path); len(hooks["PreToolUse"]) != 1 {
		t.Fatalf("uninstall left the guard behind:\n%s", raw)
	}
}

// Without clusage on PATH, install names this binary in exec form, so no shell
// has to parse a path.
func TestHookInstallExecForm(t *testing.T) {
	guardEnv(t)
	t.Setenv("PATH", t.TempDir())
	dir := t.TempDir()
	if err := manageHook("install", dir); err != nil {
		t.Fatal(err)
	}
	hooks, _ := readTestSettings(t, filepath.Join(dir, "settings.json"))
	h := hooks["PreToolUse"][0].Hooks[0]
	exe, _ := os.Executable()
	if h.Command != exe || strings.Join(h.Args, " ") != "hook run" {
		t.Errorf("hook = %+v, want %s hook run", h, exe)
	}
	if err := manageHook("status", dir); err != nil {
		t.Errorf("status: %v", err)
	}
}

func TestGuardCmd(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	off := filepath.Join(dir, "clusage-guard.off")
	if err := guardCmd([]string{"off"}); err != nil || !fileExists(off) {
		t.Fatalf("guard off: %v", err)
	}
	if err := guardCmd([]string{"on"}); err != nil || fileExists(off) {
		t.Fatalf("guard on: %v", err)
	}
	if err := guardCmd([]string{"toggle"}); err == nil {
		t.Error("an unknown action must fail")
	}
}
