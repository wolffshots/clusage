package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// hookScriptName is installed next to the binary by the package manager, and
// lives in the repo root during development.
const hookScriptName = "clusage-guard.sh"

// hookCandidates lists where the guard rail script may sit, best first, for a
// binary at exe. The unresolved path comes first on purpose: Homebrew links
// <prefix>/share/clusage at the current version, and that link survives an
// upgrade. The resolved path points into a versioned directory that an upgrade
// deletes, so it is only a fallback.
func hookCandidates(exe string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, p := range []string{exe, resolve(exe)} {
		if p == "" {
			continue
		}
		prefix := filepath.Dir(filepath.Dir(p))
		add(filepath.Join(prefix, "share", "clusage", "hooks", hookScriptName))
		add(filepath.Join(filepath.Dir(p), "hooks", hookScriptName))
	}
	add(filepath.Join("hooks", hookScriptName))
	return out
}

// resolve follows symlinks, and returns "" when it cannot.
func resolve(p string) string {
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		return ""
	}
	return r
}

// hookScript finds the guard rail script that ships with this build.
func hookScript() (string, error) {
	var candidates []string
	if p := os.Getenv("CLUSAGE_HOOK_PATH"); p != "" {
		candidates = append(candidates, p)
	}
	exe, err := os.Executable()
	if err != nil {
		exe = ""
	}
	candidates = append(candidates, hookCandidates(exe)...)

	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("cannot find %s (looked in: %v), set CLUSAGE_HOOK_PATH",
		hookScriptName, candidates)
}

// hook registers, removes, or reports the Claude Code guard rail hook.
func hook(args []string) error {
	action := ""
	if len(args) > 0 {
		action = args[0]
	}
	switch action {
	case "install", "uninstall", "status":
	default:
		return fmt.Errorf("unknown hook action %q (want: install, uninstall, status)", action)
	}

	script, err := hookScript()
	if err != nil {
		return err
	}
	cmd := exec.Command("bash", script, "--"+action)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err = cmd.Run()
	// The script has already explained itself, so exit with its code rather
	// than wrapping the failure in another message.
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		os.Exit(exit.ExitCode())
	}
	return err
}
