package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// guardEnv lists the environment variable that overrides each guard setting.
// The hook script reads the same names, so the Config tab has to resolve them
// the same way or it reports a number the guard is not using.
const (
	envSoft5h       = "CLUSAGE_GUARD_5H"
	envHard7d       = "CLUSAGE_GUARD_7D"
	envInterval     = "CLUSAGE_GUARD_INTERVAL"
	envIntervalMin  = "CLUSAGE_GUARD_INTERVAL_MIN"
	envPoll         = "CLUSAGE_GUARD_POLL"
	envMaxWait      = "CLUSAGE_GUARD_MAXWAIT"
	envAllowOverage = "CLUSAGE_GUARD_ALLOW_OVERAGE"
	envAllowTools   = "CLUSAGE_GUARD_ALLOW_TOOLS"
)

// guardConfig prints the guard settings as "key=value" lines for the hook
// script to read. The script is bash, so it cannot parse config.json, and this
// keeps the defaults and the bounds in one place.
//
// The output is read key by key rather than eval'd, so a value never reaches a
// shell as code.
func guardConfig() error {
	cfg, _, err := loadConfig()
	if err != nil {
		return err
	}
	return writeGuardConfig(os.Stdout, cfg.Guard)
}

// writeGuardConfig renders the lines the hook script reads.
func writeGuardConfig(w io.Writer, g Guard) error {
	overage := 0
	if g.AllowOverage {
		overage = 1
	}
	for _, kv := range [][2]string{
		{"soft_5h", strconv.Itoa(g.Soft5h)},
		{"hard_7d", strconv.Itoa(g.Hard7d)},
		{"interval", strconv.Itoa(g.Interval)},
		{"interval_min", strconv.Itoa(g.IntervalMin)},
		{"poll", strconv.Itoa(g.Poll)},
		{"maxwait", strconv.Itoa(g.MaxWait)},
		{"allow_overage", strconv.Itoa(overage)},
		{"allow_tools", strings.Join(g.AllowTools, " ")},
	} {
		if _, err := fmt.Fprintf(w, "%s=%s\n", kv[0], kv[1]); err != nil {
			return err
		}
	}
	return nil
}

// guardNumber resolves one guard number the way the hook script does: an
// environment variable that reads as a whole number wins over the config file.
// The second return is the variable that overrode it, empty when none did.
func guardNumber(env string, cfg int) (int, string) {
	v, ok := os.LookupEnv(env)
	if !ok {
		return cfg, ""
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return cfg, ""
	}
	return n, env
}

// guardBool resolves an on/off guard setting. Only "1" turns it on, which is
// what the hook script tests for.
func guardBool(env string, cfg bool) (bool, string) {
	v, ok := os.LookupEnv(env)
	if !ok {
		return cfg, ""
	}
	return v == "1", env
}

// guardText resolves the allow list. An empty variable reads as unset, because
// the hook script cannot express an empty list either.
func guardText(env string, cfg []string) ([]string, string) {
	v := os.Getenv(env)
	if strings.TrimSpace(v) == "" {
		return cfg, ""
	}
	return strings.Fields(v), env
}

// effectiveGuard resolves every guard setting against the environment, and
// returns the short names of the variables that overrode the file.
func effectiveGuard(g Guard) (Guard, []string) {
	var over []string
	note := func(name string) {
		if name != "" {
			over = append(over, strings.TrimPrefix(name, "CLUSAGE_GUARD_"))
		}
	}
	var name string
	g.Soft5h, name = guardNumber(envSoft5h, g.Soft5h)
	note(name)
	g.Hard7d, name = guardNumber(envHard7d, g.Hard7d)
	note(name)
	g.Interval, name = guardNumber(envInterval, g.Interval)
	note(name)
	g.IntervalMin, name = guardNumber(envIntervalMin, g.IntervalMin)
	note(name)
	g.Poll, name = guardNumber(envPoll, g.Poll)
	note(name)
	g.MaxWait, name = guardNumber(envMaxWait, g.MaxWait)
	note(name)
	g.AllowOverage, name = guardBool(envAllowOverage, g.AllowOverage)
	note(name)
	g.AllowTools, name = guardText(envAllowTools, g.AllowTools)
	note(name)
	// A bad override falls through to the file, and the hook script does the
	// same, so normalize here rather than trust the variable.
	g.normalize()
	return g, over
}

// guardStatus is what the Config tab reports about the hook itself.
type guardStatus struct {
	// Registered is true when settings.json names the guard script.
	Registered bool
	// Off is true when the off switch file exists, which stands the guard
	// down for every session on the machine.
	Off bool
	// OffPath is where that file goes, so the tab can name it.
	OffPath string
	// Disabled is true when CLUSAGE_GUARD_DISABLE turns this session off.
	Disabled bool
}

// readGuardStatus reads the Claude Code settings file and the off switch. The
// settings file is only scanned for the script name, because the hook may be
// registered on either event and under any matcher.
func readGuardStatus() guardStatus {
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return guardStatus{}
		}
		dir = filepath.Join(home, ".claude")
	}
	st := guardStatus{
		OffPath:  filepath.Join(dir, "clusage-guard.off"),
		Disabled: os.Getenv("CLUSAGE_GUARD_DISABLE") == "1",
	}
	if _, err := os.Stat(st.OffPath); err == nil {
		st.Off = true
	}
	raw, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err == nil {
		st.Registered = strings.Contains(string(raw), "clusage-guard")
	}
	return st
}
