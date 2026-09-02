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

func TestHookCandidatesPreferTheStablePrefix(t *testing.T) {
	got := hookCandidates("/opt/homebrew/bin/clusage")
	want := "/opt/homebrew/share/clusage/hooks/" + hookScriptName
	if len(got) == 0 || got[0] != want {
		t.Fatalf("hookCandidates()[0] = %q, want %q (all: %v)", got[0], want, got)
	}
}

func TestHookCandidatesFallBackToTheResolvedPath(t *testing.T) {
	// A Homebrew-shaped tree: bin/clusage is a link into a versioned dir, and
	// only the versioned dir holds the script.
	root := t.TempDir()
	cellar := filepath.Join(root, "Cellar", "clusage", "1.0.0")
	for _, d := range []string{filepath.Join(root, "bin"),
		filepath.Join(cellar, "bin"), filepath.Join(cellar, "share", "clusage", "hooks")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	real := filepath.Join(cellar, "bin", "clusage")
	if err := os.WriteFile(real, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "bin", "clusage")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	// On macOS the temp root is itself a symlink, so resolve it before
	// comparing paths.
	realCellar, err := filepath.EvalSymlinks(cellar)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(realCellar, "share", "clusage", "hooks", hookScriptName)
	got := hookCandidates(link)
	for _, p := range got {
		if p == want {
			return
		}
	}
	t.Fatalf("hookCandidates(%q) = %v, missing %q", link, got, want)
}
