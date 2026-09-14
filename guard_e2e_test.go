package main

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestGuardHookEndToEnd runs the real guard rail script against a clusage built
// from this tree and a fake API. The fixture tests cover the decisions. This
// covers the path from an HTTP status to the deny text the agent reads.
func TestGuardHookEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	if runtime.GOOS == "windows" {
		t.Skip("the hook is a bash script, and Claude Code runs it on macOS and Linux")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash on PATH")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go on PATH")
	}
	bin := t.TempDir()
	if out, err := exec.Command(goBin, "build", "-o", filepath.Join(bin, "clusage"), ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	// hook runs one PreToolUse check with a fresh config, database and stamp,
	// so no backoff or cached reading carries over between cases.
	hook := func(usageStatus, probeStatus int) string {
		t.Helper()
		var usageCalls, probeCalls int
		fakeAPI(t, usageStatus, probeStatus, &usageCalls, &probeCalls)
		dir := t.TempDir()
		cfgDir := filepath.Join(dir, "config", "clusage")
		if err := os.MkdirAll(cfgDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cfgDir, "config.json"),
			[]byte(`{"source": "usage", "fallback": "probe"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(bash, filepath.Join("hooks", hookScriptName))
		cmd.Env = append(os.Environ(),
			"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
			"XDG_CONFIG_HOME="+filepath.Join(dir, "config"),
			"CLAUDE_CONFIG_DIR="+filepath.Join(dir, "claude"),
			"CLUSAGE_GUARD_STATE="+filepath.Join(dir, "stamp"),
		)
		cmd.Stdin = strings.NewReader(`{"tool_name": "Bash"}`)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("hook: %v", err)
		}
		return string(out)
	}

	// A 429 on the usage endpoint falls back to the probe, which reports room,
	// so the call passes with no output.
	if out := hook(http.StatusTooManyRequests, http.StatusOK); out != "" {
		t.Errorf("429 with a probe fallback: want an allow, got %s", out)
	}

	// A rejected login denies, and the deny names the cause and the fix.
	out := hook(http.StatusUnauthorized, http.StatusUnauthorized)
	var deny struct {
		HookSpecificOutput struct {
			PermissionDecision       string `json:"permissionDecision"`
			PermissionDecisionReason string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &deny); err != nil {
		t.Fatalf("the deny is not valid JSON: %v\n%s", err, out)
	}
	reason := deny.HookSpecificOutput.PermissionDecisionReason
	if deny.HookSpecificOutput.PermissionDecision != "deny" ||
		!strings.Contains(reason, "The error from clusage: the API rejected the OAuth token (401 Unauthorized). Run claude to log in again.") {
		t.Errorf("401: want a deny that names the fix, got %s", out)
	}
}
