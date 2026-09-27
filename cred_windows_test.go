//go:build windows

package main

import (
	"testing"

	"github.com/danieljoos/wincred"
)

// The credential store has no test double, so the round trip writes a real
// entry under a test target and deletes it after.
func TestCredSaveRoundTrip(t *testing.T) {
	const target = "clusage-test"
	if err := credSave(target, "sk-ant-oat01-test"); err != nil {
		t.Fatalf("credSave() = %v", err)
	}
	t.Cleanup(func() {
		if c, err := wincred.GetGenericCredential(target); err == nil {
			c.Delete()
		}
	})
	if got := credPassword(target); got != "sk-ant-oat01-test" {
		t.Errorf("credPassword() = %q, want the written token", got)
	}
	if got := credPassword("clusage-test-missing"); got != "" {
		t.Errorf("credPassword(missing) = %q, want \"\"", got)
	}
}
