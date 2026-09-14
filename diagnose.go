package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go/option"
)

// ---- traces ----------------------------------------------------------------

// traceKeep is how many HTTP calls the traces table keeps.
const traceKeep = 200

// trace is one HTTP call to the API. An HTTP error keeps its body in
// fetch_errors, so Err only holds a failure that got no response at all.
type trace struct {
	At        time.Time
	Source    string // usage or probe
	Token     string // where the token came from, never the token
	Status    int    // 0 when no response came back
	Duration  time.Duration
	RequestID string
	// Skew is the server's Date header minus the local clock. HasSkew is false
	// when the response carried no Date header.
	Skew    time.Duration
	HasSkew bool
	Err     string
}

func (t trace) ok() bool { return t.Err == "" && t.Status >= 200 && t.Status < 300 }

// traced records every HTTP call a client makes. A failed write is dropped,
// because a trace must never fail the read it describes.
func traced(db *sql.DB, source, where string) option.RequestOption {
	return option.WithMiddleware(func(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
		start := time.Now()
		resp, err := next(req)
		tr := trace{At: start, Source: source, Token: where, Duration: time.Since(start)}
		if resp != nil {
			tr.Status = resp.StatusCode
			tr.RequestID = resp.Header.Get("request-id")
			if d, perr := http.ParseTime(resp.Header.Get("Date")); perr == nil {
				tr.Skew, tr.HasSkew = time.Until(d), true
			}
		}
		if err != nil {
			tr.Err = err.Error()
		}
		_ = saveTrace(db, tr)
		return resp, err
	})
}

// saveTrace records one call and deletes all but the last traceKeep.
func saveTrace(db *sql.DB, t trace) error {
	var skew any
	if t.HasSkew {
		skew = t.Skew.Milliseconds()
	}
	res, err := db.Exec(`INSERT INTO traces (at, source, token, status, duration_ms, request_id, skew_ms, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		t.At.UTC().Format(time.RFC3339Nano), t.Source, t.Token, t.Status,
		t.Duration.Milliseconds(), t.RequestID, skew, t.Err)
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	_, err = db.Exec(`DELETE FROM traces WHERE id <= ?`, id-traceKeep)
	return err
}

// recentTraces returns up to n calls, newest first.
func recentTraces(db *sql.DB, n int) ([]trace, error) {
	rows, err := db.Query(`SELECT at, source, token, status, duration_ms, request_id, skew_ms, error
		FROM traces ORDER BY id DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []trace
	for rows.Next() {
		var at string
		var ms int64
		var skew sql.NullInt64
		var t trace
		if err := rows.Scan(&at, &t.Source, &t.Token, &t.Status, &ms, &t.RequestID, &skew, &t.Err); err != nil {
			return nil, err
		}
		t.At, _ = time.Parse(time.RFC3339Nano, at)
		t.Duration = time.Duration(ms) * time.Millisecond
		t.Skew, t.HasSkew = time.Duration(skew.Int64)*time.Millisecond, skew.Valid
		out = append(out, t)
	}
	return out, rows.Err()
}

// fetchError is one row of fetch_errors.
type fetchError struct {
	At      time.Time
	Source  string
	Status  int
	RetryAt time.Time // zero when the failure set no wait
	Message string
}

// recentFetchErrors returns up to n failed reads, newest first.
func recentFetchErrors(db *sql.DB, n int) ([]fetchError, error) {
	rows, err := db.Query(`SELECT at, source, status, retry_at, message
		FROM fetch_errors ORDER BY id DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []fetchError
	for rows.Next() {
		var at, retry string
		var e fetchError
		if err := rows.Scan(&at, &e.Source, &e.Status, &retry, &e.Message); err != nil {
			return nil, err
		}
		e.At, _ = time.Parse(time.RFC3339Nano, at)
		e.RetryAt, _ = time.Parse(time.RFC3339Nano, retry)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---- where tokens live -----------------------------------------------------

// tokenLocation is one place a token can live. It splits the Claude Code login
// into its keychain entry and its file, which tokenSources reads as one source,
// so the Diagnostics tab can say which of the two holds what.
type tokenLocation struct {
	where string
	short string // for a table column
	// read returns a bare token, or Claude Code login JSON when login is true.
	// "" means the place holds nothing.
	read  func() (string, error)
	login bool
}

// errNoKeychain marks a keychain location on a platform with no keychain.
var errNoKeychain = errors.New("no macOS keychain on this platform")

// tokenLocations lists every place a token can live. A test replaces it,
// because the real list runs the keychain.
var tokenLocations = []tokenLocation{
	{"CLAUDE_CODE_OAUTH_TOKEN", "env CLAUDE_CODE_OAUTH_TOKEN",
		func() (string, error) { return os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"), nil }, false},
	{"the " + keychainService + " keychain entry", "keychain " + keychainService,
		keychainRead(keychainService), false},
	{"the " + claudeCodeKeychainService + " keychain entry", "keychain " + claudeCodeKeychainService,
		keychainRead(claudeCodeKeychainService), true},
	{"the Claude Code credentials file", ".credentials.json", credentialsFileRead, true},
}

// shortWhere shortens a token source name, as a trace stores it, for a table.
func shortWhere(where string) string {
	switch where {
	case "CLAUDE_CODE_OAUTH_TOKEN":
		return "env"
	case "the " + keychainService + " keychain entry":
		return "keychain " + keychainService
	case "the Claude Code login":
		return "claude login"
	}
	return where
}

func keychainRead(service string) func() (string, error) {
	return func() (string, error) {
		if _, err := exec.LookPath("security"); err != nil {
			return "", errNoKeychain
		}
		return keychainPassword(service), nil
	}
}

func credentialsFileRead() (string, error) {
	dir, err := claudeDir()
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".credentials.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	return string(raw), err
}

// tokenRow is what the Diagnostics tab says about one token location. It
// never holds the token itself.
type tokenRow struct {
	Where, Short, State, Fingerprint, Scopes, UsedBy string
	Present                                          bool
	Login                                            *claudeLogin // nil unless the place holds a login
	Narrow                                           bool
	HasProfile                                       bool // the login lists user:profile
}

// readTokenRows describes every token location, and marks the token each
// source would send first.
func readTokenRows(db *sql.DB, now time.Time) []tokenRow {
	// The picks follow tokenSources, the order a read really uses.
	var usagePick, probePick string
	for _, src := range tokenSources {
		t, err := src.load()
		if err != nil || t == "" {
			continue
		}
		if probePick == "" {
			probePick = tokenFingerprint(t)
		}
		if usagePick == "" && narrowToken(db, t) == nil {
			usagePick = tokenFingerprint(t)
		}
	}

	var rows []tokenRow
	for _, loc := range tokenLocations {
		r := tokenRow{Where: loc.where, Short: loc.short, State: "none", Fingerprint: "-", Scopes: "-", UsedBy: "-"}
		raw, err := loc.read()
		switch {
		case errors.Is(err, errNoKeychain):
			r.State = "n/a, no keychain"
		case err != nil:
			r.State = "error: " + err.Error()
		case strings.TrimSpace(raw) == "":
		default:
			token := strings.TrimSpace(raw)
			if loc.login {
				l, perr := parseClaudeLogin([]byte(raw), loc.where)
				if perr != nil {
					r.State = "error: " + perr.Error()
					break
				}
				token = l.AccessToken
				l.AccessToken = "" // the row must not carry the secret
				r.Login = &l
			}
			if token == "" {
				r.State = "no access token"
				break
			}
			r.Present = true
			r.Fingerprint = tokenFingerprint(token)
			r.Narrow = narrowToken(db, token) != nil
			r.State = "set"
			r.Scopes = "unknown"
			if r.Login != nil {
				r.State = expiryLabel(r.Login.ExpiresAt, now)
				r.HasProfile = slices.Contains(r.Login.Scopes, "user:profile")
				switch {
				case len(r.Login.Scopes) == 0:
					r.Scopes = "not listed"
				case r.HasProfile:
					r.Scopes = "profile yes"
				default:
					r.Scopes = "profile NO"
				}
			}
			if r.Narrow {
				r.Scopes = "narrow (recorded)"
			}
			var used []string
			if r.Fingerprint == usagePick {
				used = append(used, "usage")
			}
			if r.Fingerprint == probePick {
				used = append(used, "probe")
			}
			if len(used) > 0 {
				r.UsedBy = strings.Join(used, ", ")
			}
		}
		rows = append(rows, r)
	}
	return rows
}

// expiryLabel renders unix milliseconds as "expires in 5h12m" or "expired 3h
// ago". 0 means the login carries no expiry.
func expiryLabel(ms int64, now time.Time) string {
	if ms <= 0 {
		return "no expiry"
	}
	t := time.UnixMilli(ms)
	if now.After(t) {
		return "expired " + ageLabel(now.Sub(t)) + " ago"
	}
	return "expires in " + ageLabel(t.Sub(now))
}

// ageLabel renders a duration as "45s", "12m" or "3h5m".
func ageLabel(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	if d < time.Minute {
		return strconv.Itoa(int(d.Seconds())) + "s"
	}
	return shortDur(d.Round(time.Minute))
}

// ---- status line setup -----------------------------------------------------

// statuslineSetup is how settings.json runs the status line.
type statuslineSetup struct {
	// Mode is "direct" when the command runs clusage statusline, "wrapper" when
	// it runs a script that does, "other" for any other command, and "none".
	Mode    string
	Command string
}

func readStatuslineSetup() statuslineSetup {
	dir, err := claudeDir()
	if err != nil {
		return statuslineSetup{Mode: "none"}
	}
	raw, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		return statuslineSetup{Mode: "none"}
	}
	var s struct {
		StatusLine struct {
			Command string `json:"command"`
		} `json:"statusLine"`
	}
	cmd := ""
	if json.Unmarshal(raw, &s) == nil {
		cmd = strings.TrimSpace(s.StatusLine.Command)
	}
	switch {
	case cmd == "":
		return statuslineSetup{Mode: "none"}
	case strings.Contains(cmd, "clusage statusline"):
		return statuslineSetup{Mode: "direct", Command: cmd}
	}
	// A wrapper script: the command names a file, perhaps after sh or bash,
	// and that file runs clusage statusline.
	f := strings.Fields(cmd)
	path := f[0]
	if (path == "sh" || path == "bash") && len(f) > 1 {
		path = f[1]
	}
	if home, err := os.UserHomeDir(); err == nil {
		path = strings.Replace(path, "$HOME", home, 1)
		if strings.HasPrefix(path, "~/") {
			path = filepath.Join(home, path[2:])
		}
	}
	if body, err := os.ReadFile(path); err == nil && strings.Contains(string(body), "clusage statusline") {
		return statuslineSetup{Mode: "wrapper", Command: cmd}
	}
	return statuslineSetup{Mode: "other", Command: cmd}
}

// ---- the diagnosis ---------------------------------------------------------

const (
	sugInfo = iota
	sugWarn
	sugError
)

type suggestion struct {
	Level int
	Text  string
}

type diagSection struct {
	Title string
	Lines []string
}

// diagnosis is everything the Diagnostics tab and clusage doctor show. It is
// plain text, so both render it the same way, and it never holds a token.
type diagnosis struct {
	At          time.Time
	Suggestions []suggestion
	Sections    []diagSection
}

// diagnose reads the whole setup. A part that cannot be read says so in its
// own lines, so one broken table does not hide the rest.
func diagnose(db *sql.DB, cfg Config, cfgPath string, now time.Time) diagnosis {
	d := diagnosis{At: now}
	var sugs []suggestion
	suggest := func(level int, format string, args ...any) {
		sugs = append(sugs, suggestion{level, fmt.Sprintf(format, args...)})
	}

	validSource := slices.Contains(sources, cfg.Source)
	var chain []string
	if validSource {
		chain = cfg.chain()
	}
	uses := func(src string) bool { return slices.Contains(chain, src) }

	// ---- setup
	var setup [][2]string
	setup = append(setup, [2]string{"config", tilde(cfgPath)})
	switch {
	case !validSource:
		setup = append(setup, [2]string{"source chain", "not set"})
		suggest(sugError, `Set "source" in config.json to statusline, usage, probe or auto. Run clusage help to compare them.`)
	default:
		setup = append(setup, [2]string{"source chain", strings.Join(chain, " -> ")})
	}
	if cfg.Fallback != "" && (cfg.Fallback == "auto" || !slices.Contains(sources, cfg.Fallback)) {
		suggest(sugError, `"fallback" is %q. Set it to statusline, usage, probe, or "" for none.`, cfg.Fallback)
	}
	wait := "none"
	if until, ok := backoffUntil(db, "usage"); ok && now.Before(until) {
		wait = "until " + until.Local().Format("15:04:05") + " (" + ageLabel(until.Sub(now)) + ")"
		if uses("usage") {
			suggest(sugInfo, "The usage source is waiting out a 429 until %s. Reads go to the next source until then.", until.Local().Format("15:04"))
		}
	}
	setup = append(setup, [2]string{"usage 429 wait", wait})

	for _, g := range []struct{ label, where string }{
		{"last usage", `model = 'usage-api'`},
		{"last statusline", `model = 'statusline'`},
		{"last probe", `model NOT IN ('usage-api', 'statusline')`},
	} {
		var at string
		label := "never"
		if db.QueryRow(`SELECT fetched_at FROM readings WHERE `+g.where+` ORDER BY id DESC LIMIT 1`).Scan(&at) == nil {
			if t, err := time.Parse(time.RFC3339Nano, at); err == nil {
				label = t.Local().Format("Mon 15:04:05") + " (" + ageLabel(now.Sub(t)) + " ago)"
				if g.label == "last statusline" && uses("statusline") && now.Sub(t) > 24*time.Hour {
					suggest(sugInfo, "No status line reading in the last 24 hours. The status line only reports while a Claude Code session runs and gets responses.")
				}
			}
		} else if g.label == "last statusline" && uses("statusline") {
			suggest(sugInfo, "No status line reading yet. Start a Claude Code session and send one message.")
		}
		setup = append(setup, [2]string{g.label, label})
	}

	sl := readStatuslineSetup()
	slLabel := map[string]string{
		"direct":  "runs clusage statusline",
		"wrapper": "a wrapper script runs clusage statusline",
		"other":   "another command: " + sl.Command,
		"none":    "not set",
	}[sl.Mode]
	setup = append(setup, [2]string{"status line", slLabel})
	if uses("statusline") {
		switch sl.Mode {
		case "none":
			suggest(sugWarn, `The source chain reads the status line, but settings.json has no statusLine. Set it to {"type": "command", "command": "clusage statusline"}.`)
		case "other":
			suggest(sugWarn, "The source chain reads the status line, but settings.json runs another command. Keep it and add clusage with a wrapper script. See the README, section Status line.")
		}
	}

	gs := readGuardStatus()
	hook := "not registered"
	if gs.Registered {
		hook = "registered"
	}
	if gs.Off {
		hook += ", off switch present"
		if gs.Registered {
			suggest(sugInfo, "The guard rail is off, because %s exists. Remove it to turn the guard back on.", tilde(gs.OffPath))
		}
	}
	if gs.Disabled {
		hook += ", CLUSAGE_GUARD_DISABLE=1"
	}
	setup = append(setup, [2]string{"guard hook", hook})

	for _, s := range []struct{ name, value string }{{"fetch_cron", cfg.FetchCron}, {"probe_cron", cfg.ProbeCron}} {
		label := "(unset)"
		if strings.TrimSpace(s.value) != "" {
			label = s.value
			if !cronValid(s.value) {
				label += "  (does not parse)"
				suggest(sugWarn, "%s %q does not parse, so it never fires. Each expression needs 5 fields.", s.name, s.value)
			}
		}
		setup = append(setup, [2]string{s.name, label})
	}
	d.Sections = append(d.Sections, diagSection{"Setup", kvLines(setup)})

	// ---- tokens
	tokenRows := readTokenRows(db, now)
	table := [][]string{{"location", "state", "scopes", "used by"}}
	anyToken, usageToken := false, false
	var login *tokenRow
	for i, r := range tokenRows {
		table = append(table, []string{r.Short, r.State, r.Scopes, r.UsedBy})
		anyToken = anyToken || r.Present
		usageToken = usageToken || strings.Contains(r.UsedBy, "usage")
		if r.Login != nil && r.Present && login == nil {
			login = &tokenRows[i]
		}
	}
	tokenLines := formatTable(table)
	var more [][2]string
	for _, r := range tokenRows {
		if r.Present {
			more = append(more, [2]string{r.Short, "fingerprint " + r.Fingerprint})
		}
		if r.Login != nil && r.Present {
			more = append(more, [2]string{"", "scopes " + orDash(strings.Join(r.Login.Scopes, " "))})
			if r.Login.RefreshTokenExpiresAt > 0 {
				more = append(more, [2]string{"", "refresh token " + expiryLabel(r.Login.RefreshTokenExpiresAt, now)})
			}
		}
	}
	if len(more) > 0 {
		tokenLines = append(tokenLines, "")
		tokenLines = append(tokenLines, kvLines(more)...)
	}
	tokenLines = append(tokenLines, "", "A fingerprint is the first 16 hex digits of a SHA-256 of the token.",
		"The usage source skips a narrow token. \"profile\" is the user:profile scope it needs.")
	d.Sections = append(d.Sections, diagSection{"Tokens", tokenLines})

	if uses("usage") || uses("probe") {
		if !anyToken {
			suggest(sugError, "No token found. Log in with claude, or set CLAUDE_CODE_OAUTH_TOKEN.")
		}
	}
	if uses("usage") {
		for _, r := range tokenRows {
			if r.Login == nil || !r.Present {
				continue
			}
			switch {
			case r.Narrow:
				suggest(sugError, "The Claude Code login in %s is recorded as lacking user:profile, so the usage source never sends it. If that is wrong, run: sqlite3 ~/.config/clusage/clusage.db 'delete from narrow_tokens'", r.Where)
			case r.Login.expired(now):
				suggest(sugWarn, "The Claude Code login in %s %s, so the usage source cannot use it. Run claude to refresh it. The desktop app does not refresh this login.", r.Where, r.State)
			case r.Login.ExpiresAt > 0 && time.UnixMilli(r.Login.ExpiresAt).Sub(now) < time.Hour:
				suggest(sugInfo, "The Claude Code login in %s %s. Run claude before then to keep the usage source working.", r.Where, r.State)
			case len(r.Login.Scopes) > 0 && !r.HasProfile:
				suggest(sugWarn, "The Claude Code login in %s lists no user:profile scope, so the usage endpoint refuses it. Log in again with claude.", r.Where)
			}
		}
		if anyToken && !usageToken {
			suggest(sugWarn, "No token can read the usage endpoint, so the usage source fails on every read. Log in with claude to get a login with user:profile.")
		}
	}

	// ---- calls
	traces, terr := recentTraces(db, traceKeep)
	var callLines []string
	if terr != nil {
		callLines = []string{"could not read traces: " + terr.Error()}
	} else {
		callLines = callsLines(traces, now)
		var usageFails, probeCalls int
		for _, t := range traces {
			if now.Sub(t.At) > 24*time.Hour {
				break
			}
			switch {
			case t.Source == "usage" && !t.ok():
				usageFails++
			case t.Source == "probe":
				probeCalls++
			}
		}
		if uses("usage") && uses("probe") && usageFails > 0 && probeCalls > 0 {
			suggest(sugInfo, "In the last 24 hours the usage endpoint failed %d times and the probe made %d calls. Each probe call costs a small inference call and has no Opus-only or Sonnet-only windows.", usageFails, probeCalls)
		}
		for _, t := range traces {
			if !t.HasSkew {
				continue
			}
			if t.Skew > 30*time.Second || t.Skew < -30*time.Second {
				suggest(sugWarn, "The local clock differs from the API clock by %s. Reset times and reading ages will be off by that much.", ageLabel(t.Skew))
			}
			break
		}
	}
	d.Sections = append(d.Sections, diagSection{"Calls", callLines})

	// ---- errors
	var errLines []string
	counts, cerr := fetchErrorCounts(db, now.Add(-errorSpan))
	recent, rerr := recentFetchErrors(db, 10)
	switch {
	case cerr != nil:
		errLines = append(errLines, "could not count errors: "+cerr.Error())
	case len(counts) == 0:
		errLines = append(errLines, "no failed reads in the last 24 hours")
	default:
		var parts []string
		top := counts[0]
		for _, c := range counts {
			parts = append(parts, fmt.Sprintf("%s %s x%d", c.Source, statusLabel(c.Status), c.N))
			if c.N > top.N {
				top = c
			}
		}
		errLines = append(errLines, "last 24 hours: "+strings.Join(parts, ", "))
		suggest(sugInfo, "%d failed reads in the last 24 hours, most of them %s %s. The Errors table below has the detail.",
			errorTotal(counts), top.Source, statusLabel(top.Status))
	}
	if rerr != nil {
		errLines = append(errLines, "could not read errors: "+rerr.Error())
	} else if len(recent) > 0 {
		t := [][]string{{"when", "source", "status", "wait until", "message"}}
		for _, e := range recent {
			until := "-"
			if !e.RetryAt.IsZero() {
				until = e.RetryAt.Local().Format("15:04:05")
			}
			t = append(t, []string{e.At.Local().Format("Mon 15:04:05"), e.Source, statusLabel(e.Status), until, squash(e.Message, 70)})
		}
		errLines = append(errLines, "")
		errLines = append(errLines, formatTable(t)...)
	}
	d.Sections = append(d.Sections, diagSection{"Errors", errLines})

	// ---- latest reading
	d.Sections = append(d.Sections, diagSection{"Latest reading", readingLines(db, cfg, traces, now)})

	// ---- account and build
	var acct [][2]string
	if login != nil {
		acct = append(acct,
			[2]string{"subscription", orDash(login.Login.SubscriptionType)},
			[2]string{"rate limit tier", orDash(login.Login.RateLimitTier)},
		)
	} else {
		acct = append(acct, [2]string{"account", "no Claude Code login found"})
	}
	acct = append(acct,
		[2]string{"version", version},
		[2]string{"go", runtime.Version() + " " + runtime.GOOS + "/" + runtime.GOARCH},
	)
	rev, when, dirty := "not recorded", "", ""
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.time":
				when = "  " + s.Value
			case "vcs.modified":
				if s.Value == "true" {
					dirty = "  (modified)"
				}
			}
		}
	}
	acct = append(acct, [2]string{"commit", rev + when + dirty})
	d.Sections = append(d.Sections, diagSection{"Account and build", kvLines(acct)})

	// ---- database and guard
	d.Sections = append(d.Sections, diagSection{"Database and guard", storeLines(db, cfgPath, traces, now)})

	if len(sugs) == 0 {
		suggest(sugInfo, "Nothing to fix.")
	}
	sort.SliceStable(sugs, func(i, j int) bool { return sugs[i].Level > sugs[j].Level })
	d.Suggestions = sugs
	return d
}

// callsLines renders the last good and bad call per source, the latency, and
// the most recent calls.
func callsLines(traces []trace, now time.Time) []string {
	if len(traces) == 0 {
		return []string{"no calls recorded yet"}
	}
	row := func(label string, t trace) []string {
		status := statusLabel(t.Status)
		if t.Err != "" {
			status = "no response"
		}
		return []string{label, t.Source, t.At.Local().Format("Mon 15:04:05"), ageLabel(now.Sub(t.At)) + " ago",
			status, durLabel(t.Duration), shortWhere(t.Token), orDash(t.RequestID)}
	}
	last := [][]string{{"", "source", "when", "age", "status", "took", "token", "request-id"}}
	for _, src := range []string{"usage", "probe"} {
		var good, bad *trace
		for i := range traces {
			t := &traces[i]
			if t.Source != src {
				continue
			}
			if t.ok() && good == nil {
				good = t
			}
			if !t.ok() && bad == nil {
				bad = t
			}
		}
		if good != nil {
			last = append(last, row("last ok", *good))
		}
		if bad != nil {
			last = append(last, row("last failed", *bad))
		}
	}
	lines := formatTable(last)

	lines = append(lines, "")
	for _, src := range []string{"usage", "probe"} {
		var ds []time.Duration
		for _, t := range traces {
			if t.Source == src {
				ds = append(ds, t.Duration)
			}
		}
		if len(ds) == 0 {
			continue
		}
		slices.Sort(ds)
		lines = append(lines, fmt.Sprintf("%s latency: median %s, slowest %s, over %d calls",
			src, durLabel(ds[len(ds)/2]), durLabel(ds[len(ds)-1]), len(ds)))
	}

	recent := [][]string{{"when", "source", "status", "took", "token", "error", "request-id"}}
	for i, t := range traces {
		if i == 10 {
			break
		}
		recent = append(recent, []string{t.At.Local().Format("15:04:05"), t.Source, statusLabel(t.Status),
			durLabel(t.Duration), shortWhere(t.Token), orDash(squash(t.Err, 50)), orDash(t.RequestID)})
	}
	lines = append(lines, "", "most recent calls:")
	return append(lines, formatTable(recent)...)
}

// readingLines renders the latest reading for the configured sources, its
// windows in local time, UTC and epoch, every stored header, and the clock
// skew the last call measured.
func readingLines(db *sql.DB, cfg Config, traces []trace, now time.Time) []string {
	r, ok, err := latestReadingFrom(db, cfg.readingModels(cfg.Model)...)
	switch {
	case err != nil:
		return []string{"could not read: " + err.Error()}
	case !ok:
		return []string{"no reading yet"}
	}
	pairs := [][2]string{
		{"from", readingSource(r)},
		{"fetched", r.FetchedAt.Local().Format("Mon 2006-01-02 15:04:05") + "  " +
			r.FetchedAt.UTC().Format(time.RFC3339) + "  " + strconv.FormatInt(r.FetchedAt.Unix(), 10) +
			"  (" + ageLabel(now.Sub(r.FetchedAt)) + " ago)"},
	}
	skew := "not measured yet"
	for _, t := range traces {
		if t.HasSkew {
			// The Date header counts whole seconds, so under a second is noise.
			skew = fmt.Sprintf("API clock %+.0fs from local, measured %s ago", t.Skew.Seconds(), ageLabel(now.Sub(t.At)))
			break
		}
	}
	lines := kvLines(append(pairs, [2]string{"clock skew", skew}))
	win := [][]string{{"window", "used", "status", "resets local", "resets UTC", "epoch"}}
	for _, w := range parseWindows(r.Headers) {
		local, utc, epoch := "-", "-", "-"
		if t, ok := w.resetTime(); ok {
			local, utc, epoch = t.Local().Format("Mon 15:04"), t.UTC().Format(time.RFC3339), strconv.FormatInt(t.Unix(), 10)
		}
		used := "-"
		if f, ok := w.utilFrac(); ok {
			used = strconv.FormatFloat(f*100, 'f', 1, 64) + "%"
		}
		win = append(win, []string{w.Name, used, orDash(w.Status), local, utc, epoch})
	}
	lines = append(lines, "")
	lines = append(lines, formatTable(win)...)

	keys := make([]string, 0, len(r.Headers))
	for k := range r.Headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	lines = append(lines, "", "stored headers:")
	for _, k := range keys {
		lines = append(lines, "  "+k+": "+r.Headers[k])
	}
	return lines
}

// storeLines renders the database size and row counts, and the guard rail
// hook's last check from its stamp file.
func storeLines(db *sql.DB, cfgPath string, traces []trace, now time.Time) []string {
	path := filepath.Join(filepath.Dir(cfgPath), "clusage.db")
	var size int64
	for _, p := range []string{path, path + "-wal"} {
		if fi, err := os.Stat(p); err == nil {
			size += fi.Size()
		}
	}
	pairs := [][2]string{
		{"database", tilde(path)},
		{"size", fmt.Sprintf("%.1f KiB with the write-ahead log", float64(size)/1024)},
	}
	var counts []string
	for _, table := range []string{"readings", "calls", "fetch_errors", "traces", "narrow_tokens"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			counts = append(counts, table+" ?")
			continue
		}
		counts = append(counts, fmt.Sprintf("%s %d", table, n))
	}
	pairs = append(pairs, [2]string{"rows", strings.Join(counts, ", ")})

	stamp := os.Getenv("CLUSAGE_GUARD_STATE")
	if stamp == "" {
		dir := os.Getenv("TMPDIR")
		if dir == "" {
			dir = "/tmp"
		}
		user := os.Getenv("USER")
		if user == "" {
			user = "x"
		}
		stamp = filepath.Join(dir, "clusage-guard-"+user+".stamp")
	}
	check := "no check recorded"
	// The hook writes "<unix seconds> <5h> <7d> <5h rate> <7d rate>".
	if raw, err := os.ReadFile(stamp); err == nil {
		if f := strings.Fields(string(raw)); len(f) >= 3 {
			if secs, err := strconv.ParseInt(f[0], 10, 64); err == nil {
				t := time.Unix(secs, 0)
				check = fmt.Sprintf("%s (%s ago), saw 5h %s%% and 7d %s%%", t.Local().Format("Mon 15:04:05"), ageLabel(now.Sub(t)), f[1], f[2])
				if len(f) >= 5 {
					check += fmt.Sprintf(", rates %s and %s %%/h", f[3], f[4])
				}
			}
		}
	}
	pairs = append(pairs, [2]string{"guard stamp", tilde(stamp)}, [2]string{"last guard check", check})
	return kvLines(pairs)
}

// ---- rendering -------------------------------------------------------------

// formatTable pads each column to its widest cell. The first row is the header.
func formatTable(rows [][]string) []string {
	var widths []int
	for _, r := range rows {
		for i, c := range r {
			if i == len(widths) {
				widths = append(widths, 0)
			}
			widths[i] = max(widths[i], len([]rune(c)))
		}
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		var b strings.Builder
		for i, c := range r {
			if i < len(r)-1 {
				c += strings.Repeat(" ", widths[i]-len([]rune(c))+2)
			}
			b.WriteString(c)
		}
		out = append(out, strings.TrimRight(b.String(), " "))
	}
	return out
}

// kvLines renders label and value pairs with the values in one column.
func kvLines(pairs [][2]string) []string {
	w := 0
	for _, p := range pairs {
		w = max(w, len(p[0]))
	}
	out := make([]string, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, p[0]+strings.Repeat(" ", w-len(p[0])+2)+p[1])
	}
	return out
}

// statusLabel renders an HTTP status, or "error" for a failure with none.
func statusLabel(status int) string {
	if status == 0 {
		return "error"
	}
	return strconv.Itoa(status)
}

func durLabel(d time.Duration) string {
	if d < time.Second {
		return strconv.FormatInt(d.Milliseconds(), 10) + "ms"
	}
	return strconv.FormatFloat(d.Seconds(), 'f', 1, 64) + "s"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// squash joins a message onto one line and cuts it to n runes.
func squash(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-3]) + "..."
	}
	return s
}

var suggestionLabels = [...]string{sugInfo: "info ", sugWarn: "warn ", sugError: "error"}

// text renders the diagnosis as plain text, for clusage doctor.
func (d diagnosis) text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "clusage doctor, %s\n\nSuggestions\n", d.At.Local().Format("Mon 2006-01-02 15:04:05 MST"))
	for _, s := range d.Suggestions {
		fmt.Fprintf(&b, "  %s  %s\n", suggestionLabels[s.Level], s.Text)
	}
	for _, sec := range d.Sections {
		fmt.Fprintf(&b, "\n%s\n", sec.Title)
		for _, l := range sec.Lines {
			b.WriteString(strings.TrimRight("  "+l, " ") + "\n")
		}
	}
	return b.String()
}

// doctor is the "clusage doctor" command. It prints the Diagnostics tab as
// plain text, so a bug report can paste it. No token is ever part of it.
func doctor() error {
	cfg, path, err := loadConfig()
	if err != nil {
		return err
	}
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	fmt.Print(diagnose(db, cfg, path, time.Now()).text())
	return nil
}
