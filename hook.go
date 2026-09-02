package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// hookScriptName is installed next to the binary by the package manager, and
// lives in the repo root during development.
const hookScriptName = "clusage-guard.sh"

// brewPrefixPath rewrites a path inside a Homebrew Cellar to the same path
// under the prefix, and returns "" for anything else:
//
//	/opt/homebrew/Cellar/clusage/0.4.1/share/clusage/hooks/guard.sh
//	-> /opt/homebrew/share/clusage/hooks/guard.sh
//
// Homebrew repoints <prefix>/share/clusage on every upgrade, so that path
// stays valid while the Cellar path dies with the version.
func brewPrefixPath(p string) string {
	const marker = "/Cellar/"
	i := strings.Index(p, marker)
	if i < 0 {
		return ""
	}
	rest := strings.Split(p[i+len(marker):], "/")
	if len(rest) < 3 { // formula name, version, then the path itself
		return ""
	}
	return filepath.Join(p[:i], filepath.Join(rest[2:]...))
}

// hookCandidates lists where the guard rail script may sit, best first, for a
// binary at exe. A prefix path comes before the Cellar path it was derived
// from, so a link made to it survives an upgrade. macOS reports the resolved
// image path in os.Executable, so the rewrite above does the real work here.
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
		for _, c := range []string{
			filepath.Join(prefix, "share", "clusage", "hooks", hookScriptName),
			filepath.Join(filepath.Dir(p), "hooks", hookScriptName),
		} {
			add(brewPrefixPath(c))
			add(c)
		}
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
