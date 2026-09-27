//go:build windows

package main

import (
	"fmt"
	"os"
	"testing"

	"github.com/danieljoos/wincred"
)

// testTarget names a Credential Manager entry for one test. The store has no
// test double, so a test writes a real entry. The pid keeps two runs apart,
// and the name keeps a run away from the real clusage entry.
func testTarget(t *testing.T) string {
	target := fmt.Sprintf("clusage-test-%s-%d", t.Name(), os.Getpid())
	t.Cleanup(func() {
		if c, err := wincred.GetGenericCredential(target); err == nil {
			c.Delete()
		}
	})
	return target
}

func TestCredSaveRoundTrip(t *testing.T) {
	target := testTarget(t)
	if err := credSave(target, "sk-ant-oat01-first"); err != nil {
		t.Fatalf("credSave() = %v", err)
	}
	if got, err := credRead(target); err != nil || got != "sk-ant-oat01-first" {
		t.Fatalf("credRead() = %q, %v, want the written token", got, err)
	}

	// setup run twice must replace the token, not fail or keep the old one.
	if err := credSave(target, "sk-ant-oat01-second"); err != nil {
		t.Fatalf("credSave() over an entry = %v", err)
	}
	if got, err := credRead(target); err != nil || got != "sk-ant-oat01-second" {
		t.Errorf("credRead() after a rewrite = %q, %v, want the new token", got, err)
	}
}

func TestCredReadMissing(t *testing.T) {
	// A missing entry is how most users start, so it must read as no token,
	// not as an error that the Diagnostics tab would print.
	got, err := credRead(testTarget(t))
	if err != nil || got != "" {
		t.Errorf("credRead(missing) = %q, %v, want \"\" and no error", got, err)
	}
}

func TestCredSaveEntryShape(t *testing.T) {
	// The entry must outlive the logon session, or the token is gone after a
	// reboot, and must hold the token as plain bytes, which credRead expects.
	target := testTarget(t)
	if err := credSave(target, "sk-ant-oat01-shape"); err != nil {
		t.Fatalf("credSave() = %v", err)
	}
	c, err := wincred.GetGenericCredential(target)
	if err != nil {
		t.Fatalf("GetGenericCredential() = %v", err)
	}
	if c.Persist != wincred.PersistLocalMachine {
		t.Errorf("Persist = %v, want PersistLocalMachine", c.Persist)
	}
	if string(c.CredentialBlob) != "sk-ant-oat01-shape" {
		t.Errorf("CredentialBlob = %q, want the token bytes", c.CredentialBlob)
	}
}
