//go:build !windows

package main

import (
	"errors"
	"strings"
	"testing"
)

// With no security command on PATH, as on Linux, the store is absent. An empty
// PATH stands in for that on macOS too, so this never touches the keychain.
func TestCredNoKeychain(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	err := credSave("clusage-test", "sk-ant-oat01-test")
	if err == nil {
		t.Fatal("credSave() with no keychain returned no error")
	}
	if !strings.Contains(err.Error(), "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Errorf("credSave() error %q does not point at another way to give a token", err)
	}

	if _, err := credRead("clusage-test"); !errors.Is(err, errNoKeychain) {
		t.Errorf("credRead() = %v, want errNoKeychain so Diagnostics shows n/a", err)
	}
	// The token source must treat a missing store as no token, not a failure,
	// or every read on Linux would report an error for it.
	if got, err := storeToken(); err != nil || got != "" {
		t.Errorf("storeToken() = %q, %v, want \"\" and no error", got, err)
	}
}
