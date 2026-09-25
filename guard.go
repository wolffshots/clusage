package main

// The guard rail hook. Claude Code runs "clusage hook run" before each tool
// call and on a resumed session. The hook handles two events.
//
// On PreToolUse it pauses tool calls while the 5h rate limit window sits at or
// above a soft threshold, and denies them once a 7d window passes a hard
// threshold. A read that yields no usable window denies too, because an
// unknown budget is not the same as a free one.
//
// On SessionStart it reports what a resumed session costs to re-send, but only
// once the prompt cache behind it has expired. Claude Code measures that and
// passes it in, so this path reads nothing and adds no probe.
//
// Every degraded path denies. A dead hook lets the tool call through, which is
// the one outcome the guard exists to prevent, so hookRun recovers a panic
// into a deny as well.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// guardSleep and guardRead are variables so the tests can skip the pause and
// count the reads.
var (
	guardSleep = time.Sleep
	guardRead  = readGuardRows
)

// usageRow is one row of the usage table, as clusage usage prints it. The
// percent is rounded the way the table rounds it, so the hook judges the same
// number the user sees.
type usageRow struct {
	name   string
	pct    float64
	status string
	rate   string // percent per hour as the table prints it, "" when unknown
	reset  string // "resets ..." text, "" when unknown
}

// rowsFromReading builds the rows clusage usage would print for u.
func rowsFromReading(u usageRead) []usageRow {
	var rows []usageRow
	for _, w := range parseWindows(u.r.Headers) {
		f, ok := w.utilFrac()
		if !ok {
			continue // the table prints no percent, so the row has no number
		}
		pct, _ := strconv.ParseFloat(strconv.FormatFloat(f*100, 'f', 0, 64), 64)
		rate, ok := burnRate(u.hist, w.Name, u.now)
		reset := formatReset(w.Reset, u.now)
		if !strings.HasPrefix(reset, "resets") {
			reset = ""
		}
		rows = append(rows, usageRow{w.Name, pct, w.Status,
			strings.TrimSuffix(rateLabel(rate, ok), "%/h"), reset})
	}
	return rows
}

// parseRows reads usage table text, for CLUSAGE_GUARD_FIXTURE. A row needs a
// percent in its second field. The rate is the field ending in "%/h", found by
// scanning rather than by position, so a table with no rate column reads as an
// unknown rate. A row with no status puts the next field where the status
// goes, which is "resets" or the rate, and neither is a status.
func parseRows(text string) []usageRow {
	var rows []usageRow
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || !strings.HasSuffix(f[1], "%") {
			continue
		}
		r := usageRow{name: f[0], pct: awkNum(strings.TrimSuffix(f[1], "%"))}
		if len(f) > 3 && f[3] != "resets" && !strings.HasSuffix(f[3], "%/h") {
			r.status = f[3]
		}
		for i := 3; i < len(f); i++ {
			if r.rate == "" && strings.HasSuffix(f[i], "%/h") {
				r.rate = strings.TrimSuffix(f[i], "%/h")
			}
			if f[i] == "resets" {
				r.reset = strings.Join(f[i:], " ")
				break
			}
		}
		rows = append(rows, r)
	}
	return rows
}

// errReason is the error in a usage read, joined onto one line. An error
// starts with "clusage: ", and every line after that belongs to it. The cut
// keeps a long API body out of the deny, and the cause comes first.
func errReason(text string) string {
	var parts []string
	on := false
	for _, l := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		on = on || strings.HasPrefix(l, "clusage: ")
		if on {
			parts = append(parts, strings.NewReplacer("\t", " ", "\r", " ").Replace(l))
		}
	}
	s := []rune(strings.Join(parts, " "))
	return string(s[:min(len(s), 600)])
}

// readGuardRows reads usage for the guard. cacheMin is how old a stored
// reading may be. The second return is the error, as errReason renders it.
func readGuardRows(cacheMin int) ([]usageRow, string) {
	if fx := os.Getenv("CLUSAGE_GUARD_FIXTURE"); fx != "" {
		raw, _ := os.ReadFile(fx)
		return parseRows(string(raw)), errReason(string(raw))
	}
	fail := func(err error) ([]usageRow, string) {
		return nil, errReason("clusage: " + err.Error())
	}
	cfg, cfgPath, err := loadConfig()
	if err != nil {
		return fail(err)
	}
	cfg.ThresholdMinutes = cacheMin
	db, err := openDB()
	if err != nil {
		return fail(err)
	}
	defer db.Close()
	u, err := readReport(db, cfg, cfgPath, cfg.Model, false)
	if err != nil {
		return fail(err)
	}
	if !u.cached {
		u.save(db, cfg.Model)
	}
	return rowsFromReading(u), ""
}

// win picks the row the guard judges for one window prefix: the highest
// matching row. An exhausted status wins over the one on that row, because a
// sibling window such as 7d-opus can be spent at a low percent. ok is false
// when no row matched.
func win(rows []usageRow, prefix string) (usageRow, bool) {
	var best *usageRow
	spent := ""
	for i, r := range rows {
		if !strings.HasPrefix(r.name, prefix) {
			continue
		}
		if spent == "" && r.status != "" && !strings.HasPrefix(r.status, "allowed") {
			spent = r.status
		}
		if best == nil || r.pct > best.pct {
			best = &rows[i]
		}
	}
	if best == nil {
		return usageRow{pct: -1}, false
	}
	w := *best
	if spent != "" {
		w.status = spent
	}
	return w, true
}

// awkNum reads the number at the start of s, and 0 when there is none, the way
// awk coerces a string. A stamp or a fixture field can hold anything.
var numPrefix = regexp.MustCompile(`^[-+]?(\d+\.?\d*|\.\d+)([eE][-+]?\d+)?`)

func awkNum(s string) float64 {
	f, _ := strconv.ParseFloat(numPrefix.FindString(strings.TrimSpace(s)), 64)
	return f
}

// fmtNum prints a number without a trailing ".0", as awk does.
func fmtNum(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// ---- the poll interval -----------------------------------------------------

// intervalFor is the seconds to wait before the next check. Each window is
// measured against its own cut, and the closer of the two drives the wait. The
// ramp is quadratic, so it stays near Interval while there is headroom and
// falls to IntervalMin at the cut. Every probe spends real usage, so a slow
// ramp at low usage is the point. A negative percent reads as no load.
//
// ponytail: a percent is a level, not a rate. project below tightens the wait
// when a rate is known.
func intervalFor(p5, p7 float64, g Guard) int {
	hi, lo := float64(g.Interval), float64(g.IntervalMin)
	if hi <= 0 {
		return 0 // zero means check on every call
	}
	lo = min(max(lo, 0), hi) // a floor above the ceiling is a typo
	a, b := 0.0, 0.0
	if g.Soft5h > 0 {
		a = p5 / float64(g.Soft5h)
	}
	if g.Hard7d > 0 {
		b = p7 / float64(g.Hard7d)
	}
	x := min(max(a, b, 0), 1)
	return max(int(hi-(hi-lo)*x*x+0.5), 1)
}

// project is the seconds until a window reaches cut at rate, divided by four
// so four checks land before it rather than one. ok is false when the rate is
// not positive or the cut is already behind, because there is then nothing to
// project.
func project(p, rate, cut float64) (int, bool) {
	if rate <= 0 || p < 0 || cut <= p {
		return 0, false
	}
	return max(int((cut-p)/rate*3600/4+0.5), 1), true
}

// ---- the stamp ---------------------------------------------------------------

// stampPath is where the last check is recorded. Every session on the machine
// shares it, so the tests point CLUSAGE_GUARD_STATE at their own file.
func stampPath() string {
	if p := os.Getenv("CLUSAGE_GUARD_STATE"); p != "" {
		return p
	}
	name := os.Getenv("USER")
	if name == "" {
		if u, err := user.Current(); err == nil {
			// Windows reports DOMAIN\name, and a backslash cannot go in a file name.
			name = u.Username[strings.LastIndex(u.Username, `\`)+1:]
		}
	}
	if name == "" {
		name = "x"
	}
	return filepath.Join(os.TempDir(), "clusage-guard-"+name+".stamp")
}

// stamp is "<unix time> <5h percent> <7d percent> <5h rate> <7d rate>". An
// older build wrote fewer fields, and a truncated write leaves nothing usable,
// so a missing field reads as unknown, which is -1.
type stamp struct {
	at             int64
	p5, p7, r5, r7 float64
}

func readStamp(path string) stamp {
	s := stamp{p5: -1, p7: -1, r5: -1, r7: -1}
	raw, _ := os.ReadFile(path)
	lines := strings.SplitN(string(raw), "\n", 2)
	f := strings.Fields(lines[0])
	if len(f) > 0 {
		if n, err := strconv.ParseUint(f[0], 10, 63); err == nil {
			s.at = int64(n)
		}
	}
	for i, p := range []*float64{&s.p5, &s.p7, &s.r5, &s.r7} {
		if len(f) > i+1 {
			*p = awkNum(f[i+1])
		}
	}
	return s
}

// mark records when the check ran and what it saw, so the next call sizes its
// own wait from the same numbers.
func mark(path string, d decision) {
	rate := func(r string) string {
		if r == "" {
			return "-1"
		}
		return r
	}
	_ = os.WriteFile(path, []byte(fmt.Sprintf("%d %s %s %s %s\n", time.Now().Unix(),
		fmtNum(d.five.pct), fmtNum(d.seven.pct), rate(d.five.rate), rate(d.seven.rate))), 0o644)
}

// ---- the verdict -------------------------------------------------------------

type decision struct {
	verdict     string   // OK, SOFT, HARD, SPENT or NODATA
	name        string   // the window the verdict names, 5h or 7d
	w           usageRow // that window
	five, seven usageRow
	reason      string // NODATA only: the error from the read
}

// verdict judges the rows against the cuts. A missing 5h or 7d row is NODATA,
// because a missing 7d row leaves the hard cut unenforced.
func verdict(rows []usageRow, g Guard) decision {
	five, ok5 := win(rows, "5h")
	seven, ok7 := win(rows, "7d")
	if !ok5 || !ok7 {
		return decision{verdict: "NODATA"}
	}
	d := decision{verdict: "OK", name: "5h", five: five, seven: seven}
	spent := func(w usageRow) bool { return w.status != "" && !strings.HasPrefix(w.status, "allowed") }
	if !g.AllowOverage {
		if spent(five) {
			d.verdict = "SPENT"
		} else if spent(seven) {
			d.verdict, d.name = "SPENT", "7d"
		}
	}
	if d.verdict == "OK" {
		if seven.pct >= float64(g.Hard7d) {
			d.verdict, d.name = "HARD", "7d"
		} else if five.pct >= float64(g.Soft5h) {
			d.verdict = "SOFT"
		}
	}
	d.w = five
	if d.name == "7d" {
		d.w = seven
	}
	return d
}

// ---- the deny text -------------------------------------------------------------

type guardRail struct {
	g      Guard
	stderr io.Writer
	// handoff is where this session's handoff goes, nil when the option is
	// off.
	handoff *handoffPlan
}

// deny renders a PreToolUse deny. Every deny names the tools that still pass,
// because a denied agent cannot find that out by trying. The list is appended
// here, at the one choke point, so no deny can drift out of step with the list
// the gate enforces.
func (gr guardRail) deny(msg string) string {
	if len(gr.g.AllowTools) > 0 {
		msg += " The guard still allows these tools, so use them to ask the user and to book a retry: " +
			strings.Join(gr.g.AllowTools, ", ") + "."
	}
	type specific struct {
		HookEventName            string `json:"hookEventName"`
		PermissionDecision       string `json:"permissionDecision"`
		PermissionDecisionReason string `json:"permissionDecisionReason"`
	}
	return encodeJSON(struct {
		HookSpecificOutput specific `json:"hookSpecificOutput"`
	}{specific{"PreToolUse", "deny", msg}})
}

// encodeJSON renders v on one line. HTML escaping is off, so a "<" in an
// error reaches the agent as it was written.
func encodeJSON(v any) string {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	_ = e.Encode(v)
	return b.String()
}

// trend names the rate and when the window fills at it. It is empty when the
// rate is unknown or not positive.
func trend(p float64, rate string) string {
	r := awkNum(rate)
	if r <= 0 {
		return ""
	}
	left := max((100-p)/r*60, 0)
	return fmt.Sprintf(" It is rising at %.1f%%/h, so it fills in about %dm.", r, int(left+0.5))
}

// overage says how the user takes the pay-overage option a deny offers. The
// guard denies every tool not on the allow list, so the agent cannot stand
// the guard down itself. The off switch is a command the user can run from
// inside the session.
const overage = "If the user picks overage, the guard has to stand down first, and you cannot do that yourself, because the guard denies the call. Give the user this command to run: clusage guard off. Then tell the user to run clusage guard on once the work is done, because the guard stays off until then."

// retry tells the caller what to do about the wait. It never tells the agent
// to park itself. A wait of hours is the user's decision, so the agent asks
// and waits for an answer. handoff says how to write the handoff, and ""
// leaves that option out.
func retry(window, reset, handoff string) string {
	if reset == "" {
		if handoff == "" {
			return "The " + window + " window reported no reset time, so there is nothing to wait for. Stop all work now, in this agent and in every subagent. Report the limit to the user, and ask whether to stop here or keep working and pay overage. Wait for the answer. Do not decide it yourself. " + overage
		}
		return "The " + window + " window reported no reset time, so there is nothing to wait for. Stop all work now, in this agent and in every subagent. Report the limit to the user, and ask whether to write the current state to a handoff file and stop, keep working and pay overage, or stop here. Wait for the answer. Do not decide it yourself. " + handoff + " " + overage
	}
	options := "Offer three options: wait for the reset and resume then, keep working now and pay overage, or stop here."
	steps := ""
	if handoff != "" {
		options = "Offer four options: wait for the reset and resume then, write the current state to a handoff file so a fresh session can resume from it and stop, keep working now and pay overage, or stop here."
		steps = " " + handoff
	}
	return "It " + reset + ". Stop all other work now, in this agent and in every subagent. Do not run clusage again to check it, and trust that time. Do not schedule a resume before the user answers. Ask the user first, as a short multiple choice question, and wait for the answer. " + options + " Set a timer or a wake-up only if the user picks the first option. If the user waits, chain the wake-ups. A wake-up caps at one hour, and a gap over 55 minutes expires the prompt cache. Use legs of 55 minutes or less. Put the leg number, the total, and the reset time into the message the wake-up delivers back to you. On waking, read the leg number from that message. On an interim leg, schedule the next leg and do nothing else. Then end the turn. Never call a tool to check the clock, because a tool call can be denied." + steps + " " + overage
}

// stop denies a call that overage would pay for.
func (gr guardRail) stop(window, status string) string {
	return gr.deny("clusage guard rail: the " + window + " window is exhausted (status " + status + "), so overage is paying for this call. Stop all work now, in this agent and in every subagent. Do not retry, because every retry spends more. Tell the user the window is exhausted and end the turn. To work on overage anyway, a terminal session can set CLUSAGE_GUARD_ALLOW_OVERAGE=1, and any session can run clusage guard off to stand the guard down until clusage guard on puts it back.")
}

// nodata denies a call when the read produced no usable window. That means
// clusage itself is broken, not that there is headroom. The reason goes into
// the deny word for word, because it names the fix, such as a login to renew.
func (gr guardRail) nodata(reason string) string {
	said, relay := "", "Tell the user that clusage is not reporting"
	if reason != "" {
		said = " The error from clusage: " + strings.TrimPrefix(reason, "clusage: ")
		relay = "Tell the user that clusage is not reporting, and pass on the error from clusage and the fix it names"
	}
	fmt.Fprintf(gr.stderr, "clusage guard rail: no usable rate limit window, usage is unknown, call denied.%s\n", said)
	return gr.deny("clusage guard rail: clusage reported no usable rate limit window, so the guard cannot tell how much budget is left, and it denies instead of assuming there is room." + said + " Stop all work now, in this agent and in every subagent. Do not retry, because the next call is denied too. " + relay + ". Give them both of these commands: clusage usage -force to see the underlying error, and clusage guard off to stand the guard down until clusage guard on puts it back. Then end the turn and wait for the user.")
}

func (gr guardRail) hard(d decision) string {
	return gr.deny(fmt.Sprintf("clusage guard rail: the 7d limit is at %s%% (hard cut at %d%%).%s %s",
		fmtNum(d.w.pct), gr.g.Hard7d, trend(d.w.pct, d.seven.rate), retry("7d", d.w.reset, gr.offerHandoff(d))))
}

func (gr guardRail) soft(d decision) string {
	return gr.deny(fmt.Sprintf("clusage guard rail: the 5h limit is at %s%% and did not drop in %ds (soft limit %d%%).%s %s",
		fmtNum(d.w.pct), gr.g.MaxWait, gr.g.Soft5h, trend(d.w.pct, d.five.rate), retry("5h", d.w.reset, gr.offerHandoff(d))))
}

// offerHandoff is how a deny offers the handoff, or "". An exhausted window
// means overage would pay for the write, which happens when the user opted in
// to overage, so the option drops out then.
func (gr guardRail) offerHandoff(d decision) string {
	if gr.handoff == nil || exhausted(d.five) || exhausted(d.seven) {
		return ""
	}
	return gr.handoff.steps()
}

// exhausted reports whether a window's status says its quota is spent.
func exhausted(w usageRow) bool {
	return w.status != "" && !strings.HasPrefix(w.status, "allowed")
}

// handoffDeny denies a handoff write the guard would otherwise pass, because a
// window is exhausted and the write would be paid for by overage.
func (gr guardRail) handoffDeny(d decision) string {
	w := d.five
	if !exhausted(w) {
		w = d.seven
	}
	return gr.deny("clusage guard rail: the " + w.name + " window is exhausted (status " + w.status + "), so overage would pay for the handoff file, and the guard keeps it for when budget is left. Stop all work now, in this agent and in every subagent. Tell the user the window is exhausted and end the turn. " + overage)
}

// handoffTools are the tools that may touch the handoff file. Write refuses
// to replace a file it has not read, so Read is on the list.
var handoffTools = []string{"Read", "Write", "Edit", "MultiEdit"}

// ---- the hook ------------------------------------------------------------------

// guardSettings resolves the guard settings as the Config tab does: the
// environment over config.json over the defaults. A fixture run skips the file,
// so a test measures the defaults and not the machine it runs on.
func guardSettings() Guard {
	g := defaultConfig.Guard
	if os.Getenv("CLUSAGE_GUARD_FIXTURE") == "" {
		if cfg, _, err := loadConfig(); err == nil {
			g = cfg.Guard
		}
	}
	g, _ = effectiveGuard(g)
	return g
}

// offPath is the off switch. It stands the guard down for every session.
func offPath() string {
	dir, err := claudeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "clusage-guard.off")
}

// readPayload reads the event Claude Code writes to stdin. A hook run by hand
// has a terminal on stdin and no payload. The read gives up after 2 seconds,
// because os.Stdin has no deadline on every platform.
func readPayload(r io.Reader) []byte {
	if f, ok := r.(*os.File); ok {
		if fi, err := f.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
			return nil
		}
	}
	done := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- b
	}()
	select {
	case b := <-done:
		return b
	case <-time.After(2 * time.Second):
		return nil
	}
}

// hookRun handles one hook event.
func hookRun(stdin io.Reader, stdout, stderr io.Writer) {
	gr := guardRail{g: defaultConfig.Guard, stderr: stderr}
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintln(stderr, "clusage guard rail: panic:", r)
			fmt.Fprint(stdout, gr.nodata(""))
		}
	}()
	payload := readPayload(stdin)
	if out, ok := resumeReport(payload); ok {
		fmt.Fprint(stdout, out)
		return
	}
	gr.g = guardSettings()
	fmt.Fprint(stdout, gr.check(payload))
}

// check runs the guard rail on a PreToolUse payload, and returns the deny, or
// "" to let the call through.
func (gr guardRail) check(payload []byte) string {
	if os.Getenv("CLUSAGE_GUARD_DISABLE") == "1" {
		return ""
	}
	// The off switch. An environment variable cannot be set for one session in
	// the desktop app, so a tripped guard would otherwise deny the work of
	// fixing it.
	if off := offPath(); off != "" {
		if _, err := os.Stat(off); err == nil {
			return ""
		}
	}
	g := gr.g
	state := stampPath()
	st := readStamp(state)
	interval := intervalFor(st.p5, st.p7, g)
	// The level ramp is the floor. A projection that says a cut is closer than
	// the ramp thinks tightens the wait. The floor applies to the candidate,
	// not the result, because intervalFor already honors its own bounds.
	for _, c := range []struct {
		p, r float64
		cut  int
	}{{st.p5, st.r5, g.Soft5h}, {st.p7, st.r7, g.Hard7d}} {
		if n, ok := project(c.p, c.r, float64(c.cut)); ok {
			interval = min(interval, max(n, g.IntervalMin))
		}
	}
	if time.Now().Unix()-st.at < int64(interval) {
		return ""
	}

	var p struct {
		ToolName  string `json:"tool_name"`
		Cwd       string `json:"cwd"`
		ToolInput struct {
			FilePath string `json:"file_path"`
		} `json:"tool_input"`
	}
	_ = json.Unmarshal(payload, &p)
	if p.ToolName != "" && slices.Contains(g.AllowTools, p.ToolName) {
		return ""
	}
	gr.handoff = planHandoff(g.Handoff, p.Cwd)
	target := ""
	if gr.handoff != nil && slices.Contains(handoffTools, p.ToolName) && p.ToolInput.FilePath != "" {
		f := p.ToolInput.FilePath
		if !filepath.IsAbs(f) && p.Cwd != "" {
			f = filepath.Join(p.Cwd, f)
		}
		if gr.handoff.allows(f) {
			target = filepath.Clean(f)
		}
	}

	// A probe older than the wait it just sized would report a stale number,
	// so the reading cache tracks the interval. Under a minute this floors to
	// zero, which is what a fast poll needs.
	judge := func(cacheMin int) decision {
		rows, reason := guardRead(cacheMin)
		d := verdict(rows, g)
		d.reason = reason
		return d
	}
	d := judge(interval / 60)
	// The handoff file passes at any cut, so the agent can leave its state for
	// a fresh session. It never passes on overage.
	if target != "" {
		switch {
		case d.verdict == "NODATA":
			return gr.nodata(d.reason)
		case exhausted(d.five) || exhausted(d.seven):
			return gr.handoffDeny(d)
		case d.verdict == "OK":
			mark(state, d)
		}
		if p.ToolName != "Read" && !samePath(target, gr.handoff.router) {
			excludeHandoff(target)
		}
		return ""
	}
	for waited := 0; ; {
		switch d.verdict {
		case "NODATA":
			return gr.nodata(d.reason)
		case "SPENT":
			return gr.stop(d.name, d.w.status)
		case "HARD":
			return gr.hard(d)
		case "OK":
			if waited > 0 {
				fmt.Fprintf(gr.stderr, "clusage guard rail: 5h usage back down to %s%%, work resumed after %ds.\n", fmtNum(d.w.pct), waited)
			}
			mark(state, d)
			return ""
		}
		// SOFT: a hook that blocks for minutes makes the session look dead, and
		// the app kills it. Wait only for a short spike, then deny.
		if waited >= g.MaxWait {
			return gr.soft(d)
		}
		guardSleep(time.Duration(g.Poll) * time.Second)
		waited += g.Poll
		d = judge(0)
	}
}

// ---- the resume report -----------------------------------------------------------

// resumeReport handles a SessionStart payload. ok is false for any other
// payload, which then goes to the guard rail.
func resumeReport(payload []byte) (out string, ok bool) {
	var p map[string]any
	if json.Unmarshal(payload, &p) != nil || p["hook_event_name"] != "SessionStart" {
		return "", false
	}
	// The event is handled from here on, whether or not it prints anything. The
	// guard rail must not run on a session start.
	if os.Getenv("CLUSAGE_RESUME_DISABLE") == "1" {
		return "", true
	}
	// Claude Code sends the four fields below on a resume or a fork, from
	// v2.1.251. An older build, a fresh session, or a live cache all mean
	// nothing to report.
	if p["prompt_cache_likely_expired"] != true {
		return "", true
	}
	secs, ok1 := jsonNum(p["seconds_since_last_response"])
	tokens, ok2 := jsonNum(p["context_tokens"])
	usd, ok3 := jsonNum(p["estimated_cache_write_usd"])
	if !ok1 || !ok2 || !ok3 {
		return "", true
	}
	size := strconv.Itoa(int(tokens))
	if int(tokens) >= 1000 {
		size = fmt.Sprintf("%.0fk", float64(int(tokens))/1000)
	}
	report := fmt.Sprintf("clusage: prompt cache expired after %dm idle. This session re-sends %s tokens, about $%.2f. Consider /compact.",
		int(secs)/60, size, usd)
	// Two channels, because no single one reaches every client. A terminal
	// shows the systemMessage. The desktop app runs Claude Code with
	// --output-format stream-json, where the message never reaches the screen,
	// so the same line is handed to Claude as context with an instruction to
	// say it.
	type specific struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext"`
	}
	return encodeJSON(struct {
		SystemMessage      string   `json:"systemMessage"`
		HookSpecificOutput specific `json:"hookSpecificOutput"`
	}{report, specific{"SessionStart", report + " Open this session by telling the user that line, in one sentence, before anything else."}}), true
}

// jsonNum reads a JSON number, or a string that holds one.
func jsonNum(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case string:
		f, err := strconv.ParseFloat(x, 64)
		return f, err == nil
	}
	return 0, false
}
