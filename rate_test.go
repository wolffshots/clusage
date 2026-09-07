package main

import (
	"testing"
	"time"
)

func TestWindowLength(t *testing.T) {
	cases := map[string]time.Duration{
		"5h":      5 * time.Hour,
		"7d":      7 * 24 * time.Hour,
		"7d-opus": 7 * 24 * time.Hour,
		"30m":     30 * time.Minute,
	}
	for name, want := range cases {
		got, ok := windowLength(name)
		if !ok || got != want {
			t.Fatalf("windowLength(%q) = %v, %v; want %v, true", name, got, ok, want)
		}
	}
	for _, name := range []string{"overage", "", "hd", "0h", "5y"} {
		if got, ok := windowLength(name); ok {
			t.Fatalf("windowLength(%q) = %v, true; want no parse", name, got)
		}
	}
}

func TestTauFor(t *testing.T) {
	if got, want := tauFor("5h"), 37*time.Minute+30*time.Second; got != want {
		t.Fatalf("tauFor(5h) = %v; want %v", got, want)
	}
	if got, want := tauFor("7d"), 21*time.Hour; got != want {
		t.Fatalf("tauFor(7d) = %v; want %v", got, want)
	}
	// A window that carries no length must still get a horizon.
	if tauFor("overage") != tauFor("5h") {
		t.Fatalf("tauFor(overage) = %v; want the 5h horizon %v", tauFor("overage"), tauFor("5h"))
	}
}
