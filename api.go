package main

import (
	"context"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// claudeCodeSystemPrompt is required for Claude Code OAuth tokens to be accepted.
const claudeCodeSystemPrompt = "You are Claude Code, Anthropic's official CLI for Claude."

// tokenUse is what one probe call cost, taken from the response usage block.
type tokenUse struct {
	Input       int64 `json:"input"`
	Output      int64 `json:"output"`
	CacheCreate int64 `json:"cache_create"`
	CacheRead   int64 `json:"cache_read"`
}

// total is every token the call was billed for, cache reads included.
func (u tokenUse) total() int64 {
	return u.Input + u.Output + u.CacheCreate + u.CacheRead
}

// cached is the part of the input that came from the prompt cache.
func (u tokenUse) cached() int64 { return u.CacheRead }

// cachedFrac is the cache read share of all input tokens, 0 when there is no input.
func (u tokenUse) cachedFrac() float64 {
	in := u.Input + u.CacheCreate + u.CacheRead
	if in <= 0 {
		return 0
	}
	return float64(u.CacheRead) / float64(in)
}

func (u tokenUse) add(v tokenUse) tokenUse {
	return tokenUse{
		Input:       u.Input + v.Input,
		Output:      u.Output + v.Output,
		CacheCreate: u.CacheCreate + v.CacheCreate,
		CacheRead:   u.CacheRead + v.CacheRead,
	}
}

// fetchUsage makes the smallest possible inference call and returns the rate
// limit headers plus what the call itself cost in tokens. The token counts are
// zero when the API rejected the call, because a rejected call carries the
// headers but no usage block.
func fetchUsage(ctx context.Context, token, model string) (map[string]string, tokenUse, error) {
	client := anthropic.NewClient(
		option.WithAuthToken(token),
		option.WithHeader("anthropic-beta", "oauth-2025-04-20"),
	)
	var raw *http.Response
	msg, err := client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(model),
		MaxTokens: 1,
		System:    []anthropic.TextBlockParam{{Text: claudeCodeSystemPrompt}},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock("1")),
		},
	}, option.WithResponseInto(&raw))
	var used tokenUse
	if msg != nil {
		used = tokenUse{
			Input:       msg.Usage.InputTokens,
			Output:      msg.Usage.OutputTokens,
			CacheCreate: msg.Usage.CacheCreationInputTokens,
			CacheRead:   msg.Usage.CacheReadInputTokens,
		}
	}
	if raw != nil {
		if h := rateLimitHeaders(raw.Header); len(h) > 0 {
			return h, used, nil
		}
	}
	if err != nil {
		return nil, used, err
	}
	return nil, used, nil
}

func rateLimitHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	for k, v := range h {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "anthropic-ratelimit-") && len(v) > 0 {
			out[lk] = v[0]
		}
	}
	return out
}

type window struct {
	Name        string
	Status      string
	Utilization string
	Reset       string
}

var windowRe = regexp.MustCompile(`^anthropic-ratelimit-unified-(.+)-(status|utilization|reset)$`)

// parseWindows groups the unified rate limit headers by window (5h, 7d, overage, ...).
func parseWindows(headers map[string]string) []window {
	byName := map[string]*window{}
	for k, v := range headers {
		m := windowRe.FindStringSubmatch(k)
		if m == nil {
			continue
		}
		w := byName[m[1]]
		if w == nil {
			w = &window{Name: m[1]}
			byName[m[1]] = w
		}
		switch m[2] {
		case "status":
			w.Status = v
		case "utilization":
			w.Utilization = v
		case "reset":
			w.Reset = v
		}
	}
	out := make([]window, 0, len(byName))
	for _, w := range byName {
		out = append(out, *w)
	}
	sort.Slice(out, func(i, j int) bool {
		if windowRank(out[i].Name) != windowRank(out[j].Name) {
			return windowRank(out[i].Name) < windowRank(out[j].Name)
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// windowRank orders the shortest window first, keeping overage and unknown names at the end.
func windowRank(name string) int {
	switch {
	case strings.HasPrefix(name, "5h"):
		return 0
	case strings.HasPrefix(name, "7d") && !strings.Contains(name, "opus"):
		return 1
	case strings.HasPrefix(name, "7d"):
		return 2
	case name == "overage":
		return 4
	default:
		return 3
	}
}

// percentUsed renders a utilization fraction (0.61) as a percentage (61%).
func percentUsed(v string) string {
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return v
	}
	return strconv.FormatFloat(f*100, 'f', -1, 64) + "% used"
}

// formatReset renders a reset header (unix seconds or RFC3339) as a local time plus time left.
func formatReset(v string, now time.Time) string {
	if v == "" {
		return ""
	}
	var t time.Time
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		t = time.Unix(secs, 0)
	} else if parsed, err := time.Parse(time.RFC3339, v); err == nil {
		t = parsed
	} else {
		return v
	}
	left := t.Sub(now).Round(time.Minute)
	if left < 0 {
		return "resets " + t.Local().Format("Mon 15:04") + " (passed)"
	}
	return "resets " + t.Local().Format("Mon 15:04") + " (in " + shortDur(left) + ")"
}

func shortDur(d time.Duration) string {
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h == 0 {
		return strconv.Itoa(m) + "m"
	}
	return strconv.Itoa(h) + "h" + strconv.Itoa(m) + "m"
}

// utilFrac parses the utilization header as a 0..1 fraction. ok is false when
// the header is missing or unparseable.
func (w window) utilFrac() (float64, bool) {
	f, err := strconv.ParseFloat(w.Utilization, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// resetTime parses the reset header, which is either unix seconds or RFC3339.
func (w window) resetTime() (time.Time, bool) {
	if w.Reset == "" {
		return time.Time{}, false
	}
	if secs, err := strconv.ParseInt(w.Reset, 10, 64); err == nil {
		return time.Unix(secs, 0), true
	}
	if t, err := time.Parse(time.RFC3339, w.Reset); err == nil {
		return t, true
	}
	return time.Time{}, false
}
