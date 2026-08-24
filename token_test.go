package main

import (
	"strings"
	"testing"
	"time"
)

// seedTokenSamples builds a plausible call history: a small probe call every
// 20 minutes, with one that read from the cache.
func seedTokenSamples(n int) []TokenSample {
	out := make([]TokenSample, n)
	base := time.Now().Add(-time.Duration(n) * 20 * time.Minute)
	for i := range out {
		u := tokenUse{Input: 12, Output: 1}
		if i%5 == 0 {
			u = tokenUse{Input: 2, Output: 1, CacheRead: 10}
		}
		out[i] = TokenSample{
			CalledAt: base.Add(time.Duration(i) * 20 * time.Minute),
			Model:    "claude-opus-5",
			Used:     u,
		}
	}
	return out
}

func TestCommas(t *testing.T) {
	cases := map[int64]string{
		0: "0", 7: "7", 42: "42", 999: "999", 1000: "1,000",
		1204: "1,204", 12345: "12,345", 1234567: "1,234,567", -4321: "-4,321",
	}
	for in, want := range cases {
		if got := commas(in); got != want {
			t.Errorf("commas(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestTokenUseArithmetic(t *testing.T) {
	u := tokenUse{Input: 10, Output: 2, CacheCreate: 5, CacheRead: 25}
	if u.total() != 42 {
		t.Errorf("total() = %d, want 42", u.total())
	}
	if u.cached() != 25 {
		t.Errorf("cached() = %d, want 25", u.cached())
	}
	// cachedFrac is a share of input only, so the output token is excluded.
	if got := u.cachedFrac(); got < 0.624 || got > 0.626 {
		t.Errorf("cachedFrac() = %v, want 0.625", got)
	}
	if (tokenUse{}).cachedFrac() != 0 {
		t.Error("cachedFrac() on an empty use must be 0, not a divide by zero")
	}
	sum := u.add(tokenUse{Input: 1, Output: 1, CacheCreate: 1, CacheRead: 1})
	if sum != (tokenUse{Input: 11, Output: 3, CacheCreate: 6, CacheRead: 26}) {
		t.Errorf("add() = %+v", sum)
	}
}

func TestTokenSeriesIsCumulative(t *testing.T) {
	ss := []TokenSample{
		{CalledAt: time.Unix(100, 0), Used: tokenUse{Input: 10}},
		{CalledAt: time.Unix(200, 0), Used: tokenUse{Input: 5, Output: 1}},
		{CalledAt: time.Unix(300, 0), Used: tokenUse{CacheRead: 4}},
	}
	cum, per, stamps := tokenSeries(ss)
	wantCum := []float64{10, 16, 20}
	wantPer := []float64{10, 6, 4}
	for i := range wantCum {
		if cum[i] != wantCum[i] {
			t.Errorf("cum[%d] = %v, want %v", i, cum[i], wantCum[i])
		}
		if per[i] != wantPer[i] {
			t.Errorf("per[%d] = %v, want %v", i, per[i], wantPer[i])
		}
	}
	if len(stamps) != 3 || !stamps[0].Equal(time.Unix(100, 0)) {
		t.Errorf("stamps = %v", stamps)
	}
}

// TestTokenStoreRoundTrip writes calls to a throwaway database and reads the
// span rows and the all-time totals back.
func TestTokenStoreRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	db, err := openDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now()
	rows := []TokenSample{
		{CalledAt: now.Add(-48 * time.Hour), Model: "m", Used: tokenUse{Input: 100, Output: 1}},
		{CalledAt: now.Add(-2 * time.Hour), Model: "m", Used: tokenUse{Input: 10, Output: 1, CacheRead: 5}},
		{CalledAt: now.Add(-1 * time.Hour), Model: "m", Used: tokenUse{Input: 12, Output: 1}},
		// A rejected call reports no usage and must not be recorded.
		{CalledAt: now, Model: "m", Used: tokenUse{}},
	}
	for _, r := range rows {
		if err := saveTokens(db, r); err != nil {
			t.Fatal(err)
		}
	}

	total, calls, err := tokenTotals(db)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3 (the zero-usage call must be dropped)", calls)
	}
	if total.total() != 130 {
		t.Errorf("all-time total = %d, want 130", total.total())
	}
	if total.CacheRead != 5 {
		t.Errorf("all-time cache read = %d, want 5", total.CacheRead)
	}

	recent, err := tokensSince(db, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 2 {
		t.Fatalf("tokensSince returned %d rows, want 2", len(recent))
	}
	if !recent[0].CalledAt.Before(recent[1].CalledAt) {
		t.Error("tokensSince must return oldest first")
	}
	if recent[0].Used.CacheRead != 5 || recent[1].Used.Input != 12 {
		t.Errorf("rows came back wrong: %+v", recent)
	}
}

// TestTokensViewEmptyState checks the tab still renders before any call is
// recorded, since an empty series would otherwise index past the end.
func TestTokensViewEmptyState(t *testing.T) {
	m := newModel(nil, defaultConfig, "/tmp/config.json", Reading{}, false)
	m.width, m.height = 96, 32
	out := m.tokensView(24)
	if !strings.Contains(out, "no calls in this span") {
		t.Errorf("empty tokens view lost its hint:\n%s", out)
	}
	if !strings.Contains(out, "no calls recorded yet") {
		t.Errorf("empty tokens view lost the all-time line:\n%s", out)
	}
}
