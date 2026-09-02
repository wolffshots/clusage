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

// hookScript finds the guard rail script that ships with this build.
func hookScript() (string, error) {
	var candidates []string
	if p := os.Getenv("CLUSAGE_HOOK_PATH"); p != "" {
		candidates = append(candidates, p)
	}
	if exe, err := os.Executable(); err == nil {
		if exe, err := filepath.EvalSymlinks(exe); err == nil {
			// Homebrew and friends: <prefix>/bin/clusage next to
			// <prefix>/share/clusage/hooks/clusage-guard.sh.
			prefix := filepath.Dir(filepath.Dir(exe))
			candidates = append(candidates,
				filepath.Join(prefix, "share", "clusage", "hooks", hookScriptName),
				filepath.Join(filepath.Dir(exe), "hooks", hookScriptName))
		}
	}
	candidates = append(candidates, filepath.Join("hooks", hookScriptName))

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
