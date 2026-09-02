package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHookScriptPrefersEnv(t *testing.T) {
	p := filepath.Join(t.TempDir(), hookScriptName)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLUSAGE_HOOK_PATH", p)
	got, err := hookScript()
	if err != nil {
		t.Fatal(err)
	}
	if got != p {
		t.Fatalf("hookScript() = %q, want %q", got, p)
	}
}

func TestHookScriptFindsRepoCopy(t *testing.T) {
	t.Setenv("CLUSAGE_HOOK_PATH", "")
	got, err := hookScript()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("hooks", hookScriptName); got != want {
		t.Fatalf("hookScript() = %q, want %q", got, want)
	}
}

func TestHookRejectsUnknownAction(t *testing.T) {
	if err := hook([]string{"enable"}); err == nil {
		t.Fatal("hook(enable) = nil, want an error")
	}
}
