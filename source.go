package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// usageAPIModel marks a reading that came from the OAuth usage endpoint.
const usageAPIModel = "usage-api"

// sources are the accepted "source" values. auto walks the other three in
// order. "fallback" takes any of them but auto.
var sources = []string{"statusline", "usage", "probe", "auto"}

// errNoSource is what a reading fails with until config.json names a source.
// There is no default, so nobody ends up on a source they did not pick.
func errNoSource(path, got string) error {
	if got == "" {
		return fmt.Errorf(`no source set: set "source" in %s to one of: %s (statusline is recommended). Run clusage help to compare them`,
			path, strings.Join(sources, ", "))
	}
	return fmt.Errorf(`unknown source %q in %s: want one of: %s. Run clusage help to compare them`, got, path, strings.Join(sources, ", "))
}

// errBadFallback is what a reading fails with when "fallback" names no single
// source. auto is not a fallback, because it is a chain of its own.
func errBadFallback(path, got string) error {
	return fmt.Errorf(`unknown fallback %q in %s: want statusline, usage, probe, or "" for none`, got, path)
}

// chain is the order readUsage tries the sources in. auto walks usage,
// statusline and probe. Any other source is tried alone, then its fallback.
func (c Config) chain() []string {
	switch {
	case c.Source == "auto":
		return []string{"usage", "statusline", "probe"}
	case c.Fallback != "" && c.Fallback != c.Source:
		return []string{c.Source, c.Fallback}
	}
	return []string{c.Source}
}

// readingModels names the models a cached reading may come from, so a status
// line row cannot stand in for a usage reading that carries more windows. nil
// means any model, which is what auto takes.
func (c Config) readingModels(model string) []string {
	if c.Source == "auto" {
		return nil
	}
	var out []string
	for _, src := range c.chain() {
		switch src {
		case "usage":
			out = append(out, usageAPIModel)
		case "probe":
			out = append(out, model)
		case "statusline":
			out = append(out, statuslineModel)
		}
	}
	return out
}

// defaultBackoff and maxBackoff bound how long a 429 with no retry-after header
// keeps the usage endpoint from being called again. See backoffFor.
const (
	defaultBackoff = time.Minute
	maxBackoff     = 15 * time.Minute
)

// readUsage gets one reading from the configured source, and tries the next
// source in the chain when one fails. fresh is false when the reading came out
// of the database, so the caller does not store it a second time.
//
// Every usage or probe failure is stored in fetch_errors, so the TUI can count
// the failures of every run, the guard rail hook's included. Every source that
// fails adds its reason to the error, so a failed chain says why each step was
// skipped rather than only the last one.
func readUsage(ctx context.Context, db *sql.DB, cfg Config, cfgPath, model string) (r Reading, used tokenUse, fresh bool, err error) {
	if !slices.Contains(sources, cfg.Source) {
		return Reading{}, tokenUse{}, false, errNoSource(cfgPath, cfg.Source)
	}
	if cfg.Fallback != "" && (cfg.Fallback == "auto" || !slices.Contains(sources, cfg.Fallback)) {
		return Reading{}, tokenUse{}, false, errBadFallback(cfgPath, cfg.Fallback)
	}

	var token string
	var tokenErr error
	tokenRead := false
	tok := func() (string, error) {
		if !tokenRead {
			token, tokenErr = loadToken()
			tokenRead = true
		}
		return token, tokenErr
	}

	chain := cfg.chain()
	var errs []error
	var last error
	for i, src := range chain {
		// The endpoint answers 429 often. Calling it again inside the wait it
		// asked for only earns another 429, so go straight to the next step.
		if src == "usage" {
			if until, ok := backoffUntil(db, src); ok && time.Now().Before(until) {
				last = fmt.Errorf("usage: waiting out a 429 until %s", until.Local().Format("15:04:05"))
				errs = append(errs, last)
				continue
			}
		}
		r, used, fresh, err := readFrom(ctx, db, cfg, src, model, i < len(chain)-1, tok)
		if err == nil {
			r.Fallback = i > 0
			return r, used, fresh, nil
		}
		if src != "statusline" {
			// A failed write must not fail the read. The count is a report, and
			// the next source may still answer.
			_ = saveFetchError(db, src, err, time.Now())
		}
		last = fmt.Errorf("%s: %w", src, err)
		errs = append(errs, last)
	}

	err = errors.Join(errs...)
	var apiErr *anthropic.Error
	if errors.As(last, &apiErr) && apiErr.StatusCode == http.StatusUnauthorized {
		// The guard rail hook shows the first line of this error to the agent,
		// so the cause and the fix lead, and the detail follows.
		err = fmt.Errorf("the API rejected the OAuth token (401 Unauthorized). Run claude to log in again. If you set CLAUDE_CODE_OAUTH_TOKEN or ran clusage setup, replace that token with a new one from claude setup-token.\n%w", err)
	}
	return Reading{}, tokenUse{}, false, err
}

// readFrom gets one reading from one source. more is true when another source
// follows in the chain, so a stale status line reading gives way to it.
func readFrom(ctx context.Context, db *sql.DB, cfg Config, src, model string, more bool, tok func() (string, error)) (Reading, tokenUse, bool, error) {
	switch src {
	case "usage":
		token, err := tok()
		if err != nil {
			return Reading{}, tokenUse{}, false, err
		}
		h, err := fetchOAuthUsage(ctx, token)
		if err != nil {
			return Reading{}, tokenUse{}, false, err
		}
		return Reading{FetchedAt: time.Now(), Model: usageAPIModel, Headers: h}, tokenUse{}, true, nil

	case "statusline":
		last, ok, err := latestReadingFrom(db, statuslineModel)
		// A row is written only when the numbers change, so a row under a minute
		// old is current even when the caller asked for no cache at all.
		maxAge := max(time.Duration(cfg.ThresholdMinutes)*time.Minute, time.Minute)
		switch {
		case err != nil:
			return Reading{}, tokenUse{}, false, err
		case !ok:
			return Reading{}, tokenUse{}, false, errors.New("no reading yet")
		// The last step in the chain takes the last reading however old,
		// because there is nothing else to fall back on. Before another step a
		// stale one gives way, because that step can read now.
		case more && time.Since(last.FetchedAt) > maxAge:
			return Reading{}, tokenUse{}, false, fmt.Errorf("last reading is %s old",
				time.Since(last.FetchedAt).Round(time.Second))
		}
		return last, tokenUse{}, false, nil

	default: // probe
		token, err := tok()
		if err != nil {
			return Reading{}, tokenUse{}, false, err
		}
		h, used, err := fetchUsage(ctx, token, model)
		switch {
		case err != nil:
			return Reading{}, tokenUse{}, false, err
		case len(h) == 0:
			return Reading{}, tokenUse{}, false, errors.New("no usable anthropic-ratelimit-unified-* headers on the response")
		}
		return Reading{FetchedAt: time.Now(), Model: model, Headers: h}, used, true, nil
	}
}

// fetchOAuthUsage reads the account's limit windows from the OAuth usage
// endpoint, the one Claude Code's /usage panel reads. It is undocumented, so
// the response is decoded loosely and a body with no usable window is an error.
//
// Retries are off for the same reason as fetchUsage: the endpoint answers 429
// often, and in auto a refusal should move on to the next source at once.
func fetchOAuthUsage(ctx context.Context, token string) (map[string]string, error) {
	client := anthropic.NewClient(
		option.WithAuthToken(token),
		option.WithHeader("anthropic-beta", "oauth-2025-04-20"),
		option.WithMaxRetries(0),
	)
	var body json.RawMessage
	if err := client.Get(ctx, "api/oauth/usage", nil, &body); err != nil {
		return nil, err
	}
	h, err := oauthUsageHeaders(body)
	if err != nil {
		return nil, err
	}
	if len(parseWindows(h)) == 0 {
		return nil, errors.New("no usage windows in the response")
	}
	return h, nil
}

type usageBucket struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    string   `json:"resets_at"`
}

type usageLimit struct {
	Kind     string   `json:"kind"`
	Percent  *float64 `json:"percent"`
	ResetsAt string   `json:"resets_at"`
	Scope    struct {
		Model struct {
			DisplayName string `json:"display_name"`
		} `json:"model"`
	} `json:"scope"`
}

// oauthUsageHeaders turns the usage endpoint body into the header map a probe
// reading stores, so the views, the history and the hook read it unchanged.
//
// The body comes in two shapes. Older accounts carry flat five_hour, seven_day,
// seven_day_opus and seven_day_sonnet buckets. Migrated accounts carry a
// limits array instead. The flat buckets win, and limits fills what they lack.
//
// A flat bucket that is present but null means no use in that window, so it
// records 0%. Dropping it would leave the hook with no 5h row, and the hook
// denies on that.
func oauthUsageHeaders(raw []byte) (map[string]string, error) {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	h := map[string]string{}
	set := func(name string, pct float64, reset string) {
		// The endpoint reports 0 to 100. The headers carry a 0 to 1 fraction.
		h["anthropic-ratelimit-unified-"+name+"-utilization"] = strconv.FormatFloat(pct/100, 'f', -1, 64)
		if reset != "" {
			h["anthropic-ratelimit-unified-"+name+"-reset"] = reset
		}
	}
	has := func(name string) bool {
		_, ok := h["anthropic-ratelimit-unified-"+name+"-utilization"]
		return ok
	}

	for key, name := range map[string]string{
		"five_hour": "5h", "seven_day": "7d",
		"seven_day_opus": "7d-opus", "seven_day_sonnet": "7d-sonnet",
	} {
		v, ok := body[key]
		if !ok {
			continue
		}
		if string(v) == "null" {
			set(name, 0, "")
			continue
		}
		var b usageBucket
		if err := json.Unmarshal(v, &b); err == nil && b.Utilization != nil {
			set(name, *b.Utilization, b.ResetsAt)
		}
	}

	var limits []usageLimit
	if v, ok := body["limits"]; ok {
		_ = json.Unmarshal(v, &limits) // a shape change here must not lose the flat buckets
	}
	for _, l := range limits {
		// A 0% entry with no reset is a placeholder, not a window.
		if l.Percent == nil || (*l.Percent == 0 && l.ResetsAt == "") {
			continue
		}
		var name string
		switch l.Kind {
		case "session":
			name = "5h"
		case "weekly_all":
			name = "7d"
		case "weekly_scoped":
			// "Opus", "Claude Opus" and "Claude Opus 4.8" all name 7d-opus: the
			// family is the first word that is neither "claude" nor a version.
			for _, f := range strings.Fields(strings.ToLower(l.Scope.Model.DisplayName)) {
				if f != "claude" && (f[0] < '0' || f[0] > '9') {
					name = "7d-" + f
					break
				}
			}
			if name == "" {
				continue
			}
		default:
			continue
		}
		if !has(name) {
			set(name, *l.Percent, l.ResetsAt)
		}
	}

	if v, ok := body["extra_usage"]; ok {
		var x struct {
			IsEnabled   bool     `json:"is_enabled"`
			Utilization *float64 `json:"utilization"`
		}
		if err := json.Unmarshal(v, &x); err == nil && x.IsEnabled && x.Utilization != nil {
			set("overage", *x.Utilization, "")
		}
	}
	return h, nil
}
