package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// hookScriptName is the legacy wrapper script. An install before the hook
// moved into Go linked it into the Claude Code hooks directory.
const hookScriptName = "clusage-guard.sh"

// hook runs, registers, removes, or reports the Claude Code guard rail hook.
func hook(args []string) error {
	action := ""
	if len(args) > 0 {
		action, args = args[0], args[1:]
	}
	arg := func(i int, def string) string {
		if i < len(args) {
			return args[i]
		}
		return def
	}
	// num reads a number the way the old script did: an empty argument takes
	// the default, and one that is not a number reads as zero.
	num := func(i int, def float64) float64 {
		if s := arg(i, ""); s != "" {
			return awkNum(s)
		}
		return def
	}
	switch action {
	case "run":
		hookRun(os.Stdin, os.Stdout, os.Stderr)
		return nil
	case "interval":
		fmt.Println(intervalFor(num(0, -1), num(1, -1), guardSettings()))
		return nil
	case "project":
		if n, ok := project(num(0, -1), num(1, 0), num(2, 0)); ok {
			fmt.Println(n)
		}
		return nil
	case "install", "uninstall", "status":
		dir, err := claudeDir()
		if err != nil {
			return err
		}
		return manageHook(action, dir)
	}
	return fmt.Errorf("unknown hook action %q (want: run, install, uninstall, status, interval, project). Run clusage help hook for details", action)
}

// guardCmd turns the guard off for every session, or back on.
func guardCmd(args []string) error {
	action := ""
	if len(args) > 0 {
		action = args[0]
	}
	path := offPath()
	switch action {
	case "off", "on":
		if err := setGuardOff(path, action == "off"); err != nil {
			return err
		}
	case "status":
	default:
		return fmt.Errorf("unknown guard action %q (want: off, on, status). Run clusage help guard for details", action)
	}
	if _, err := os.Stat(path); err == nil {
		fmt.Printf("guard off: %s exists. Run clusage guard on to put it back.\n", path)
	} else {
		fmt.Println("guard on")
	}
	return nil
}

// guardEvents lists the events the hook registers on: the event, its matcher,
// and the hook timeout. The guard rail needs the whole pause budget. The
// resume report only reads the payload it is handed, so it needs seconds.
func guardEvents(maxWait int) []struct {
	event, matcher string
	timeout        int
} {
	return []struct {
		event, matcher string
		timeout        int
	}{{"PreToolUse", "*", maxWait + 15}, {"SessionStart", "resume|fork", 10}}
}

// ownedHook reports whether a hook in settings.json is the guard rail: the
// legacy script, or clusage hook run in either the shell or the exec form.
func ownedHook(h any) bool {
	o, ok := h.(*jsonObj)
	if !ok {
		return false
	}
	line, _ := o.get("command").(string)
	if args, ok := o.get("args").([]any); ok {
		for _, a := range args {
			s, _ := a.(string)
			line += " " + s
		}
	}
	line = strings.TrimSpace(line)
	return strings.Contains(line, "clusage-guard") ||
		(strings.Contains(line, "clusage") && strings.HasSuffix(line, "hook run"))
}

// ownedEntry reports whether a settings.json entry holds a guard rail hook.
func ownedEntry(e any) bool {
	o, ok := e.(*jsonObj)
	if !ok {
		return false
	}
	hooks, _ := o.get("hooks").([]any)
	for _, h := range hooks {
		if ownedHook(h) {
			return true
		}
	}
	return false
}

// hookCommand is the command install registers. Homebrew and Scoop both put
// clusage on PATH, and that path survives an upgrade, so the bare name is the
// usual answer. "clusage hook run" reads the same in bash, sh and PowerShell,
// which are the shells Claude Code runs a hook in. Without clusage on PATH the
// hook names this binary in exec form, which runs with no shell, so the path
// needs no quoting on any platform.
func hookCommand() (command string, args []any) {
	if _, err := exec.LookPath("clusage"); err == nil {
		return "clusage hook run", nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "clusage hook run", nil
	}
	return exe, []any{"hook", "run"}
}

// setHook points one hook at the current command.
func setHook(h *jsonObj, command string, args []any, timeout int) {
	h.set("type", "command")
	h.set("command", command)
	if args != nil {
		h.set("args", args)
	} else {
		h.del("args")
	}
	h.set("timeout", json.Number(fmt.Sprint(timeout)))
}

// manageHook edits settings.json in dir. The rest of the file keeps its keys
// and their order.
func manageHook(mode, dir string) error {
	path := filepath.Join(dir, "settings.json")
	settings, err := readSettings(path)
	if err != nil {
		return err
	}
	hooks, _ := settings.get("hooks").(*jsonObj)
	if hooks == nil {
		hooks = &jsonObj{}
	}
	events := guardEvents(guardSettings().MaxWait)

	if mode == "status" {
		return hookStatus(dir, path, hooks, events)
	}

	action := "installed"
	if mode == "install" {
		command, args := hookCommand()
		for _, ev := range events {
			entries, _ := hooks.get(ev.event).([]any)
			entries = splitShared(entries)
			found := false
			// An install over an older version rewrites the entry it made, so a
			// new event is added and an old command is brought up to date.
			for _, e := range entries {
				if !ownedEntry(e) {
					continue
				}
				found = true
				o := e.(*jsonObj)
				o.set("matcher", ev.matcher)
				for _, h := range o.get("hooks").([]any) {
					if ownedHook(h) {
						setHook(h.(*jsonObj), command, args, ev.timeout)
					}
				}
			}
			if !found {
				h := &jsonObj{}
				setHook(h, command, args, ev.timeout)
				entries = append(entries, &jsonObj{{"matcher", ev.matcher}, {"hooks", []any{h}}})
			}
			hooks.set(ev.event, entries)
		}
		settings.set("hooks", hooks)
	} else {
		found := false
		for _, ev := range events {
			entries, _ := hooks.get(ev.event).([]any)
			entries = splitShared(entries)
			var keep []any
			for _, e := range entries {
				if ownedEntry(e) {
					found = true
				} else {
					keep = append(keep, e)
				}
			}
			if len(keep) > 0 {
				hooks.set(ev.event, keep)
			} else {
				hooks.del(ev.event)
			}
		}
		if len(*hooks) > 0 {
			settings.set("hooks", hooks)
		} else {
			settings.del("hooks")
		}
		action = "removed"
		if !found {
			action = "was not registered"
		}
	}
	if err := writeSettings(path, settings); err != nil {
		return err
	}
	removeLegacyLink(dir)
	fmt.Printf("clusage guard rail %s in %s\n", action, path)
	return nil
}

// splitShared takes the guard rail hooks out of an entry that also holds
// another tool's hooks, and gives them an entry of their own. install and
// uninstall then edit or drop only entries that belong to the guard, so they
// never change another tool's matcher or remove its hooks.
func splitShared(entries []any) []any {
	var out, mine []any
	for _, e := range entries {
		out = append(out, e)
		if !ownedEntry(e) {
			continue
		}
		o := e.(*jsonObj)
		var rest, ours []any
		for _, h := range o.get("hooks").([]any) {
			if ownedHook(h) {
				ours = append(ours, h)
			} else {
				rest = append(rest, h)
			}
		}
		if len(rest) > 0 {
			o.set("hooks", rest)
			mine = append(mine, &jsonObj{{"matcher", o.get("matcher")}, {"hooks", ours}})
		}
	}
	return append(out, mine...)
}

// removeLegacyLink removes the link an older install made to the hook script.
// settings.json no longer names it. A real file is the user's, so it stays.
func removeLegacyLink(dir string) {
	link := filepath.Join(dir, "hooks", hookScriptName)
	if fi, err := os.Lstat(link); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		_ = os.Remove(link)
	}
}

// hookStatus prints the registered entries and whether their command runs.
func hookStatus(dir, path string, hooks *jsonObj, events []struct {
	event, matcher string
	timeout        int
}) error {
	if off := filepath.Join(dir, "clusage-guard.off"); fileExists(off) {
		fmt.Println("off switch present:", off)
	}
	var broken []string
	found, legacy := false, false
	for _, ev := range events {
		entries, _ := hooks.get(ev.event).([]any)
		for _, e := range entries {
			if !ownedEntry(e) {
				continue
			}
			for _, h := range e.(*jsonObj).get("hooks").([]any) {
				if !ownedHook(h) {
					continue
				}
				found = true
				o := h.(*jsonObj)
				command, _ := o.get("command").(string)
				line := command
				args, isExec := o.get("args").([]any)
				for _, a := range args {
					line += fmt.Sprint(" ", a)
				}
				timeout := "60"
				if t := o.get("timeout"); t != nil {
					timeout = fmt.Sprint(t)
				}
				fmt.Printf("registered: %s %s (timeout %ss)\n", ev.event, line, timeout)
				if strings.Contains(line, "clusage-guard") {
					legacy = true
					continue
				}
				name := command
				if !isExec {
					name = strings.Fields(command)[0]
				}
				if _, err := lookPath(name); err != nil {
					broken = append(broken, name)
				}
			}
		}
	}
	if !found {
		return fmt.Errorf("not registered in %s", path)
	}
	if legacy {
		fmt.Println("note: an entry runs the old hook script. Rerun clusage hook install to register the binary.")
	}
	if len(broken) > 0 {
		return fmt.Errorf("broken: %s does not resolve to a program. Rerun clusage hook install", broken[0])
	}
	return nil
}

// lookPath is exec.LookPath, which also takes an absolute path.
var lookPath = exec.LookPath

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// ---- settings.json -----------------------------------------------------------

// jsonObj is a JSON object that keeps its key order. encoding/json into a map
// loses the order, and settings.json is a file the user edits by hand.
type jsonObj []jsonKV

type jsonKV struct {
	k string
	v any
}

func (o *jsonObj) get(k string) any {
	for _, kv := range *o {
		if kv.k == k {
			return kv.v
		}
	}
	return nil
}

func (o *jsonObj) set(k string, v any) {
	for i := range *o {
		if (*o)[i].k == k {
			(*o)[i].v = v
			return
		}
	}
	*o = append(*o, jsonKV{k, v})
}

func (o *jsonObj) del(k string) {
	for i := range *o {
		if (*o)[i].k == k {
			*o = append((*o)[:i], (*o)[i+1:]...)
			return
		}
	}
}

// readSettings reads settings.json into an ordered tree. A missing file is an
// empty object.
func readSettings(path string) (*jsonObj, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &jsonObj{}, nil
	}
	if err != nil {
		return nil, err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	v, err := decodeOrdered(d)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	o, ok := v.(*jsonObj)
	if !ok {
		return nil, fmt.Errorf("parse %s: the top level is not an object", path)
	}
	return o, nil
}

// decodeOrdered reads one JSON value. An object becomes a *jsonObj, an array
// a []any, and a number a json.Number, so nothing changes on the way back out.
func decodeOrdered(d *json.Decoder) (any, error) {
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch t {
	case json.Delim('{'):
		o := &jsonObj{}
		for d.More() {
			k, err := d.Token()
			if err != nil {
				return nil, err
			}
			v, err := decodeOrdered(d)
			if err != nil {
				return nil, err
			}
			*o = append(*o, jsonKV{k.(string), v})
		}
		_, err = d.Token()
		return o, err
	case json.Delim('['):
		a := []any{}
		for d.More() {
			v, err := decodeOrdered(d)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
		_, err = d.Token()
		return a, err
	}
	return t, nil
}

func encodeOrdered(b *bytes.Buffer, v any) {
	switch x := v.(type) {
	case *jsonObj:
		b.WriteByte('{')
		for i, kv := range *x {
			if i > 0 {
				b.WriteByte(',')
			}
			encodeOrdered(b, kv.k)
			b.WriteByte(':')
			encodeOrdered(b, kv.v)
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			encodeOrdered(b, e)
		}
		b.WriteByte(']')
	case json.Number:
		b.WriteString(string(x))
	default: // string, bool, nil
		b.WriteString(strings.TrimSuffix(encodeJSON(x), "\n"))
	}
}

// writeSettings writes through a temp file, so a write that dies partway
// leaves the old settings.json and not a short one.
func writeSettings(path string, o *jsonObj) error {
	var flat, out bytes.Buffer
	encodeOrdered(&flat, o)
	if err := json.Indent(&out, flat.Bytes(), "", "  "); err != nil {
		return err
	}
	out.WriteByte('\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp := path + ".clusage-tmp"
	if err := os.WriteFile(tmp, out.Bytes(), mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
