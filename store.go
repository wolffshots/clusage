package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	_ "modernc.org/sqlite"
)

const keychainService = "clusage"

type Config struct {
	// Source is where readings come from: "statusline" reads what "clusage
	// statusline" stored, "usage" reads the OAuth usage endpoint, "probe" sends
	// a probe call, and "auto" tries usage, statusline, probe in that order.
	// It has no default: readUsage refuses to run until it is set.
	Source string `json:"source"`
	// Fallback is the source tried when Source fails, or "" for none. It takes
	// any source but auto, and auto ignores it.
	Fallback         string `json:"fallback"`
	Model            string `json:"model"`
	ThresholdMinutes int    `json:"threshold_minutes"`
	// FetchCron is one or more 5-field cron expressions separated by ";".
	// Empty disables the TUI auto-fetch.
	FetchCron string `json:"fetch_cron"`
	// ProbeCron is a second schedule, in the same form, that reads the probe
	// source whatever Source says. A probe call starts a 5h window. Empty
	// disables it.
	ProbeCron string `json:"probe_cron"`
	// HistoryHours is how far back the history graphs read. 0 uses the default.
	HistoryHours int `json:"history_hours"`
	// Guard holds the guard rail hook's settings.
	Guard Guard `json:"guard"`
}

// Guard holds the settings of the guard rail hook. The hook is a shell script,
// so it reads them through "clusage guard-config" rather than parsing the JSON
// itself. A CLUSAGE_GUARD_* environment variable still wins over the file, so a
// single terminal session can override the machine.
type Guard struct {
	// Soft5h pauses and polls once the 5h window reaches this percent.
	Soft5h int `json:"soft_5h_percent"`
	// Hard7d denies without polling once the 7d window passes this percent.
	Hard7d int `json:"hard_7d_percent"`
	// Interval is the seconds between checks at low usage, IntervalMin the
	// seconds between checks at a threshold.
	Interval    int `json:"interval_seconds"`
	IntervalMin int `json:"interval_min_seconds"`
	// Poll is the seconds between checks while paused, MaxWait how long the
	// hook pauses before it denies.
	Poll    int `json:"poll_seconds"`
	MaxWait int `json:"max_wait_seconds"`
	// AllowOverage keeps working once a window is exhausted.
	AllowOverage bool `json:"allow_overage"`
	// AllowTools names the tools that pass without a check. An empty list
	// reads as unset, because the hook cannot express one either.
	AllowTools []string `json:"allow_tools"`
}

var defaultConfig = Config{
	Model:            "claude-haiku-4-5",
	ThresholdMinutes: 5,
	FetchCron:        "*/15 * * * *",
	HistoryHours:     168,
	Guard: Guard{
		Soft5h:       90,
		Hard7d:       95,
		Interval:     300,
		IntervalMin:  30,
		Poll:         15,
		MaxWait:      45,
		AllowOverage: false,
		AllowTools:   []string{"ScheduleWakeup", "CronCreate", "AskUserQuestion"},
	},
}

// configDir returns ~/.config/clusage, honoring XDG_CONFIG_HOME.
func configDir() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	dir := filepath.Join(base, "clusage")
	return dir, os.MkdirAll(dir, 0o700)
}

// loadConfig reads the config file, writing the defaults first if it is missing.
func loadConfig() (Config, string, error) {
	dir, err := configDir()
	if err != nil {
		return Config{}, "", err
	}
	path := filepath.Join(dir, "config.json")
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		out, _ := json.MarshalIndent(defaultConfig, "", "  ")
		// Written through a temp file in the same directory: a truncating write
		// that dies partway leaves a short config.json, and every later run
		// then fails to parse it with no repair path.
		tmp, err := os.CreateTemp(dir, "config-*.json")
		if err != nil {
			return Config{}, path, err
		}
		defer os.Remove(tmp.Name())
		if _, err := tmp.Write(append(out, '\n')); err != nil {
			tmp.Close()
			return Config{}, path, err
		}
		if err := tmp.Chmod(0o600); err != nil {
			tmp.Close()
			return Config{}, path, err
		}
		if err := tmp.Close(); err != nil {
			return Config{}, path, err
		}
		if err := os.Rename(tmp.Name(), path); err != nil {
			return Config{}, path, err
		}
		return defaultConfig, path, nil
	}
	if err != nil {
		return Config{}, path, err
	}
	cfg := defaultConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, path, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.Model == "" {
		cfg.Model = defaultConfig.Model
	}
	if cfg.ThresholdMinutes <= 0 {
		cfg.ThresholdMinutes = defaultConfig.ThresholdMinutes
	}
	if cfg.HistoryHours <= 0 {
		cfg.HistoryHours = defaultConfig.HistoryHours
	}
	cfg.Guard.normalize()
	return cfg, path, nil
}

// normalize replaces any value the hook script would reject with the default,
// so the Config tab reports the number the guard actually applies.
//
// Zero stays legal for three of them. No ceiling means check on every call, no
// floor lets the ramp reach zero, and no wait means deny at once. Poll takes a
// floor of one second, because sleep 0 never advances the pause loop.
func (g *Guard) normalize() {
	d := defaultConfig.Guard
	if g.Soft5h < 0 || g.Soft5h > 100 {
		g.Soft5h = d.Soft5h
	}
	if g.Hard7d < 0 || g.Hard7d > 100 {
		g.Hard7d = d.Hard7d
	}
	if g.Interval < 0 {
		g.Interval = d.Interval
	}
	if g.IntervalMin < 0 {
		g.IntervalMin = d.IntervalMin
	}
	if g.Poll < 1 {
		g.Poll = d.Poll
	}
	if g.MaxWait < 0 {
		g.MaxWait = d.MaxWait
	}
	if len(g.AllowTools) == 0 {
		g.AllowTools = d.AllowTools
	}
}

// saveToken stores the OAuth token in the login keychain.
//
// The token goes in over stdin rather than as a "-w <token>" argument. Command
// arguments are visible to any local process running ps for as long as the
// command runs, so an argument would expose the token to every other user on
// the machine. With -w last it prompts for the value and a confirmation, so the
// token is written twice.
func saveToken(token string) error {
	if strings.ContainsAny(token, "\r\n") {
		// The prompt reads one line per value. An embedded line break would
		// store a truncated token and still report success.
		return fmt.Errorf("token contains a line break")
	}
	cmd := exec.Command("security", "add-generic-password",
		"-a", os.Getenv("USER"), "-s", keychainService, "-U", "-w")
	cmd.Stdin = strings.NewReader(token + "\n" + token + "\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("keychain write failed: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// claudeCodeKeychainService is where Claude Code keeps its login on macOS.
const claudeCodeKeychainService = "Claude Code-credentials"

// errNoToken is what a token read fails with when no place holds a token.
var errNoToken = errors.New("no token found: log in with claude, set CLAUDE_CODE_OAUTH_TOKEN, or run clusage setup on macOS. Run clusage help setup for details")

// tokenSource is one place a token can come from. where names it in errors and
// on the Config tab. load returns "" and no error when the place holds no
// token, so the next place is tried.
type tokenSource struct {
	where string
	load  func() (string, error)
}

// tokenSources lists the places a token may come from, in the order clusage
// tries them. Tokens differ in scope: one from claude setup-token can call the
// API but not the usage endpoint, so a refused token gives way to the next.
// A test replaces the list, because the real one runs the keychain.
var tokenSources = []tokenSource{
	{"CLAUDE_CODE_OAUTH_TOKEN", func() (string, error) { return os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"), nil }},
	{"the " + keychainService + " keychain entry", func() (string, error) { return keychainPassword(keychainService), nil }},
	{"the Claude Code login", claudeCodeLoginToken},
}

// keychainPassword reads one macOS keychain entry, or "" when it is missing.
// The security command does not exist off macOS, so the read fails fast there.
func keychainPassword(service string) string {
	out, err := exec.Command("security", "find-generic-password", "-s", service, "-w").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// claudeCodeLoginToken reads the Claude Code login: the keychain on macOS, a
// file on Linux and Windows. No login at all is "" and no error.
func claudeCodeLoginToken() (string, error) {
	if raw := keychainPassword(claudeCodeKeychainService); raw != "" {
		return claudeCodeLogin([]byte(raw), "the "+claudeCodeKeychainService+" keychain entry")
	}
	t, err := claudeCodeToken()
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	return t, err
}

// tokens loads the token sources in order, each at most once, so a read can
// move on to the next token without running the keychain again.
type tokens struct {
	loaded []tokenResult
}

type tokenResult struct {
	token, where string
	err          error
}

// use calls try with each token in order. It stops at the first success, and
// at the first failure that retry does not accept. Every failure is named by
// the place its token came from. last is the final failure try returned, so a
// caller can tell why the last token was refused.
func (ts *tokens) use(try func(token string) error, retry func(error) bool) (last, err error) {
	var errs []error
	for i, src := range tokenSources {
		if i == len(ts.loaded) {
			t, err := src.load()
			ts.loaded = append(ts.loaded, tokenResult{t, src.where, err})
		}
		c := ts.loaded[i]
		if c.err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", c.where, c.err))
			continue
		}
		if c.token == "" {
			continue
		}
		last = try(c.token)
		if last == nil {
			return nil, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", c.where, last))
		if !retry(last) {
			break
		}
	}
	if len(errs) == 0 {
		return nil, errNoToken
	}
	return last, errors.Join(errs...)
}

// firstToken is the token a read tries first, and where it came from, for the
// Config tab.
func firstToken() (token, where string, err error) {
	for _, src := range tokenSources {
		t, lerr := src.load()
		switch {
		case lerr != nil && err == nil:
			err = fmt.Errorf("%s: %w", src.where, lerr)
		case lerr == nil && t != "":
			return t, src.where, nil
		}
	}
	if err == nil {
		err = errNoToken
	}
	return "", "", err
}

// claudeCodeToken reads the access token from the .credentials.json file that
// a Claude Code login writes on Linux and Windows.
func claudeCodeToken() (string, error) {
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".claude")
	}
	path := filepath.Join(dir, ".credentials.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return claudeCodeLogin(raw, path)
}

// claudeCodeLogin takes the access token out of a Claude Code login, which is
// the same JSON in the macOS keychain and in the credentials file. where names
// its origin in errors.
//
// Clusage never refreshes the token. A refresh rotates the refresh token, and
// Claude Code would lose its login. An expired token asks for a claude run,
// which refreshes it.
func claudeCodeLogin(raw []byte, where string) (string, error) {
	var c struct {
		OAuth struct {
			AccessToken string `json:"accessToken"`
			ExpiresAt   int64  `json:"expiresAt"` // unix milliseconds
		} `json:"claudeAiOauth"`
	}
	// The decode error names no content, so the token cannot leak through it.
	if err := json.Unmarshal(raw, &c); err != nil {
		return "", fmt.Errorf("parse %s: %w", where, err)
	}
	switch {
	case c.OAuth.AccessToken == "":
		return "", fmt.Errorf("no Claude Code login in %s", where)
	case c.OAuth.ExpiresAt > 0 && time.Now().UnixMilli() >= c.OAuth.ExpiresAt:
		return "", fmt.Errorf("the login in %s has expired: run claude once to refresh it", where)
	}
	return c.OAuth.AccessToken, nil
}

type Reading struct {
	FetchedAt time.Time
	Model     string
	Headers   map[string]string
	// Fallback is true when an earlier source in the chain failed first. It is
	// not stored, so a reading read back from the database never carries it.
	Fallback bool
}

// readingSource names where a reading came from, as "usage", "statusline" or
// "probe <model>", with ", fallback" when an earlier source failed first. A
// probe reading is stored under its model name, so any other model is a probe.
func readingSource(r Reading) string {
	s := "probe " + r.Model
	switch r.Model {
	case usageAPIModel:
		s = "usage"
	case statuslineModel:
		s = "statusline"
	}
	if r.Fallback {
		s += ", fallback"
	}
	return s
}

func openDB() (*sql.DB, error) {
	dir, err := configDir()
	if err != nil {
		return nil, err
	}
	// The driver only sets these when the DSN asks. Without them the database
	// opens with journal_mode=delete and busy_timeout=0, so the guard rail hook
	// and the TUI fail each other's writes instantly with SQLITE_BUSY.
	// A URI path uses forward slashes and starts with one, so a Windows path
	// becomes /C:/Users/..., which SQLite reads as the drive.
	p := filepath.ToSlash(filepath.Join(dir, "clusage.db"))
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	dsn := "file:" + (&url.URL{Path: p}).String() +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// migrate creates the tables. It is apart from openDB so a test can run it on
// an in-memory database.
func migrate(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS readings (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		fetched_at TEXT NOT NULL,
		model TEXT NOT NULL,
		headers TEXT NOT NULL
	)`)
	if err != nil {
		return err
	}
	// Separate table rather than columns on readings: a reading is written even
	// when the API rejects the call and reports no usage, so the two are not
	// always one to one.
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS calls (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		called_at TEXT NOT NULL,
		model TEXT NOT NULL,
		input INTEGER NOT NULL,
		output INTEGER NOT NULL,
		cache_create INTEGER NOT NULL,
		cache_read INTEGER NOT NULL
	)`)
	if err != nil {
		return err
	}
	// One row per failed usage or probe read. status is the HTTP status, 0 for
	// a failure with none. retry_at is empty unless the failure was a 429.
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS fetch_errors (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		at TEXT NOT NULL,
		source TEXT NOT NULL,
		status INTEGER NOT NULL,
		retry_at TEXT NOT NULL,
		message TEXT NOT NULL
	)`)
	return err
}

// errorKeep is how long a failed read stays in fetch_errors. The TUI counts
// one day, and a stuck hook can add thousands of rows a day.
const errorKeep = 7 * 24 * time.Hour

// saveFetchError records one failed read, and deletes the rows older than
// errorKeep. A 429 also records when the source may be called again.
func saveFetchError(db *sql.DB, source string, err error, now time.Time) error {
	status, retryAt := 0, ""
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		status = apiErr.StatusCode
		if status == http.StatusTooManyRequests {
			retryAt = now.Add(backoffFor(db, source, apiErr.Response, now)).UTC().Format(time.RFC3339Nano)
		}
	}
	_, err = db.Exec(`INSERT INTO fetch_errors (at, source, status, retry_at, message) VALUES (?, ?, ?, ?, ?)`,
		now.UTC().Format(time.RFC3339Nano), source, status, retryAt, err.Error())
	if err != nil {
		return err
	}
	_, err = db.Exec(`DELETE FROM fetch_errors WHERE at < ?`, now.Add(-errorKeep).UTC().Format(time.RFC3339Nano))
	return err
}

// backoffFor is how long to leave source alone after a 429. A retry-after
// header in seconds wins. Without one the wait doubles with each 429 of the
// last hour, from defaultBackoff up to maxBackoff, so an endpoint that keeps
// refusing is asked less and less often. An hour with no 429 starts over.
func backoffFor(db *sql.DB, source string, resp *http.Response, now time.Time) time.Duration {
	if resp != nil {
		if secs, err := strconv.Atoi(resp.Header.Get("retry-after")); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
	}
	var n int
	// A failed count reads as none, which gives the shortest wait.
	_ = db.QueryRow(`SELECT COUNT(*) FROM fetch_errors WHERE source = ? AND status = ? AND at >= ?`,
		source, http.StatusTooManyRequests, now.Add(-time.Hour).UTC().Format(time.RFC3339Nano)).Scan(&n)
	if wait := defaultBackoff << min(n, 8); wait < maxBackoff {
		return wait
	}
	return maxBackoff
}

// backoffUntil returns when the newest failure of source said to call again.
// ok is false when that failure set no wait, or nothing could be read.
func backoffUntil(db *sql.DB, source string) (time.Time, bool) {
	var retryAt string
	if err := db.QueryRow(`SELECT retry_at FROM fetch_errors WHERE source = ? ORDER BY id DESC LIMIT 1`,
		source).Scan(&retryAt); err != nil || retryAt == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, retryAt)
	return t, err == nil
}

// errorCount is how many reads of one source failed with one status.
type errorCount struct {
	Source string
	Status int
	N      int
}

// fetchErrorCounts groups the failures at or after since by source and status.
func fetchErrorCounts(db *sql.DB, since time.Time) ([]errorCount, error) {
	rows, err := db.Query(`SELECT source, status, COUNT(*) FROM fetch_errors
		WHERE at >= ? GROUP BY source, status ORDER BY source, status`,
		since.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []errorCount
	for rows.Next() {
		var c errorCount
		if err := rows.Scan(&c.Source, &c.Status, &c.N); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// TokenSample is one probe call and what it cost.
type TokenSample struct {
	CalledAt time.Time
	Model    string
	Used     tokenUse
}

// saveTokens records what one probe call cost. A call with no usage block is
// dropped, so a rejected call does not read as a free call.
func saveTokens(db *sql.DB, s TokenSample) error {
	if s.Used.total() == 0 {
		return nil
	}
	_, err := db.Exec(
		`INSERT INTO calls (called_at, model, input, output, cache_create, cache_read)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		s.CalledAt.UTC().Format(time.RFC3339Nano), s.Model,
		s.Used.Input, s.Used.Output, s.Used.CacheCreate, s.Used.CacheRead)
	return err
}

// tokenTotals sums every call ever recorded, and counts them.
func tokenTotals(db *sql.DB) (tokenUse, int, error) {
	var u tokenUse
	var n int
	err := db.QueryRow(`SELECT
		COALESCE(SUM(input), 0), COALESCE(SUM(output), 0),
		COALESCE(SUM(cache_create), 0), COALESCE(SUM(cache_read), 0),
		COUNT(*) FROM calls`).
		Scan(&u.Input, &u.Output, &u.CacheCreate, &u.CacheRead, &n)
	if err != nil {
		return tokenUse{}, 0, err
	}
	return u, n, nil
}

// tokensSince returns every call made at or after since, oldest first, for the
// token graphs.
func tokensSince(db *sql.DB, since time.Time) ([]TokenSample, error) {
	rows, err := db.Query(
		`SELECT called_at, model, input, output, cache_create, cache_read
		 FROM calls WHERE called_at >= ? ORDER BY id`,
		since.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TokenSample
	for rows.Next() {
		var ts string
		var s TokenSample
		if err := rows.Scan(&ts, &s.Model, &s.Used.Input, &s.Used.Output,
			&s.Used.CacheCreate, &s.Used.CacheRead); err != nil {
			return nil, err
		}
		at, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			continue // a row written by an older format; skip it rather than fail the view
		}
		s.CalledAt = at
		out = append(out, s)
	}
	return out, rows.Err()
}

func saveReading(db *sql.DB, r Reading) error {
	blob, err := json.Marshal(r.Headers)
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO readings (fetched_at, model, headers) VALUES (?, ?, ?)`,
		r.FetchedAt.UTC().Format(time.RFC3339Nano), r.Model, string(blob))
	return err
}

// latestReading returns the newest cached reading, or ok=false when the table is empty.
func latestReading(db *sql.DB) (Reading, bool, error) {
	return latestReadingFrom(db)
}

// latestReadingFrom returns the newest reading stored under any of models, or
// the newest of any model when none are named.
func latestReadingFrom(db *sql.DB, models ...string) (Reading, bool, error) {
	q := `SELECT fetched_at, model, headers FROM readings`
	args := make([]any, len(models))
	if len(models) > 0 {
		q += ` WHERE model IN (?` + strings.Repeat(`, ?`, len(models)-1) + `)`
		for i, m := range models {
			args[i] = m
		}
	}
	var ts, model, blob string
	err := db.QueryRow(q+` ORDER BY id DESC LIMIT 1`, args...).Scan(&ts, &model, &blob)
	if err == sql.ErrNoRows {
		return Reading{}, false, nil
	}
	if err != nil {
		return Reading{}, false, err
	}
	at, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return Reading{}, false, err
	}
	r := Reading{FetchedAt: at, Model: model}
	if err := json.Unmarshal([]byte(blob), &r.Headers); err != nil {
		return Reading{}, false, err
	}
	return r, true, nil
}

// readingsSince returns every reading fetched at or after since, oldest first,
// for the history graphs.
func readingsSince(db *sql.DB, since time.Time) ([]Reading, error) {
	rows, err := db.Query(
		`SELECT fetched_at, model, headers FROM readings WHERE fetched_at >= ? ORDER BY id`,
		since.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Reading
	for rows.Next() {
		var ts, model, blob string
		if err := rows.Scan(&ts, &model, &blob); err != nil {
			return nil, err
		}
		at, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			continue // a row written by an older format; skip it rather than fail the view
		}
		r := Reading{FetchedAt: at, Model: model}
		if err := json.Unmarshal([]byte(blob), &r.Headers); err != nil {
			continue
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
