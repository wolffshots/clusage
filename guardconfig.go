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
// The hook and the Config tab both resolve them through effectiveGuard, so the
// tab cannot report a number the guard is not using.
const (
	envSoft5h       = "CLUSAGE_GUARD_5H"
	envHard7d       = "CLUSAGE_GUARD_7D"
	envInterval     = "CLUSAGE_GUARD_INTERVAL"
	envIntervalMin  = "CLUSAGE_GUARD_INTERVAL_MIN"
	envPoll         = "CLUSAGE_GUARD_POLL"
	envMaxWait      = "CLUSAGE_GUARD_MAXWAIT"
	envAllowOverage = "CLUSAGE_GUARD_ALLOW_OVERAGE"
	envAllowTools   = "CLUSAGE_GUARD_ALLOW_TOOLS"
	envHandoff      = "CLUSAGE_GUARD_HANDOFF"
)

// guardConfig prints the guard settings the hook applies, as "key=value" lines:
// the environment over config.json over the defaults.
func guardConfig() error {
	cfg, _, err := loadConfig()
	if err != nil {
		return err
	}
	g, _ := effectiveGuard(cfg.Guard)
	return writeGuardConfig(os.Stdout, g)
}

// writeGuardConfig renders the guard settings as "key=value" lines.
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
		{"handoff_file", g.Handoff},
	} {
		if _, err := fmt.Fprintf(w, "%s=%s\n", kv[0], kv[1]); err != nil {
			return err
		}
	}
	return nil
}

// guardNumber resolves one guard number the way the hook does: an
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

// guardBool resolves an on/off guard setting. Only "1" turns it on.
func guardBool(env string, cfg bool) (bool, string) {
	v, ok := os.LookupEnv(env)
	if !ok {
		return cfg, ""
	}
	return v == "1", env
}

// guardText resolves the allow list. An empty variable reads as unset, so the
// default list comes back.
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
	if v := strings.TrimSpace(os.Getenv(envHandoff)); v != "" {
		g.Handoff = v
		note(envHandoff)
	}
	// A bad override falls through to the file, so normalize here rather than
	// trust the variable.
	g.normalize()
	return g, over
}

// guardStatus is what the Config tab reports about the hook itself.
type guardStatus struct {
	// Registered is true when settings.json names the guard rail hook.
	Registered bool
	// Legacy is true when an entry still runs the old hook script.
	Legacy bool
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
	settings, err := readSettings(filepath.Join(dir, "settings.json"))
	if err != nil {
		return st
	}
	hooks, _ := settings.get("hooks").(*jsonObj)
	if hooks == nil {
		return st
	}
	for _, ev := range *hooks {
		entries, _ := ev.v.([]any)
		for _, e := range entries {
			if !ownedEntry(e) {
				continue
			}
			st.Registered = true
			for _, h := range e.(*jsonObj).get("hooks").([]any) {
				if !ownedHook(h) {
					continue
				}
				if c, _ := h.(*jsonObj).get("command").(string); strings.Contains(c, "clusage-guard") {
					st.Legacy = true
				}
			}
		}
	}
	return st
}

// setGuardOff creates the off switch file when off is true and removes it
// otherwise. The caller reads the status back from disk, so the screen shows
// what the file system holds, not what was asked for.
func setGuardOff(path string, off bool) error {
	if path == "" {
		return fmt.Errorf("no home directory, so the off switch has no place")
	}
	if off {
		return os.WriteFile(path, nil, 0o644)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
