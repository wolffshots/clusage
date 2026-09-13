package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// usageAPIModel marks a reading that came from the OAuth usage endpoint.
const usageAPIModel = "usage-api"

// sources are the accepted config values. auto walks the other three in order.
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

// readUsage gets one reading from the configured source. auto tries the usage
// endpoint, then a status line reading no older than the threshold, then the
// probe call. fresh is false when the reading came out of the database, so the
// caller does not store it a second time.
//
// Every source that fails adds its reason to the error, so a failed auto run
// says why each step was skipped rather than only the last one.
func readUsage(ctx context.Context, db *sql.DB, cfg Config, cfgPath, model string) (r Reading, used tokenUse, fresh bool, err error) {
	if !slices.Contains(sources, cfg.Source) {
		return Reading{}, tokenUse{}, false, errNoSource(cfgPath, cfg.Source)
	}
	try := func(src string) bool { return cfg.Source == "auto" || cfg.Source == src }
	var errs []error

	var token string
	var tokenErr error
	if try("usage") || try("probe") {
		token, tokenErr = loadToken()
	}

	if try("usage") {
		if tokenErr != nil {
			errs = append(errs, fmt.Errorf("usage: %w", tokenErr))
		} else if h, err := fetchOAuthUsage(ctx, token); err != nil {
			errs = append(errs, fmt.Errorf("usage: %w", err))
		} else {
			return Reading{FetchedAt: time.Now(), Model: usageAPIModel, Headers: h}, tokenUse{}, true, nil
		}
	}

	if try("statusline") {
		last, ok, err := latestReadingFrom(db, statuslineModel)
		maxAge := time.Duration(cfg.ThresholdMinutes) * time.Minute
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("statusline: %w", err))
		case !ok:
			errs = append(errs, errors.New("statusline: no reading yet"))
		// An explicit statusline source takes the last reading however old,
		// because there is nothing else to fall back on. In auto a stale one
		// gives way to the probe, which can read now.
		case cfg.Source == "auto" && time.Since(last.FetchedAt) > maxAge:
			errs = append(errs, fmt.Errorf("statusline: last reading is %s old",
				time.Since(last.FetchedAt).Round(time.Second)))
		default:
			return last, tokenUse{}, false, nil
		}
	}

	if try("probe") {
		if tokenErr != nil {
			errs = append(errs, fmt.Errorf("probe: %w", tokenErr))
		} else {
			h, used, err := fetchUsage(ctx, token, model)
			switch {
			case err != nil:
				errs = append(errs, fmt.Errorf("probe: %w", err))
			case len(h) == 0:
				errs = append(errs, errors.New("probe: no usable anthropic-ratelimit-unified-* headers on the response"))
			default:
				return Reading{FetchedAt: time.Now(), Model: model, Headers: h}, used, true, nil
			}
		}
	}

	return Reading{}, tokenUse{}, false, errors.Join(errs...)
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
