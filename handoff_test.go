package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile writes a test file, making its directory.
func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// payloadFor is a PreToolUse payload for tool on file, from a session in cwd.
func payloadFor(tool, cwd, file string) string {
	return `{"tool_name":"` + tool + `","cwd":` + strings.TrimSpace(encodeJSON(cwd)) +
		`,"tool_input":{"file_path":` + strings.TrimSpace(encodeJSON(file)) + `}}`
}

const trackerRouter = "# App\n\n## References\n\n| Read this | When |\n|---|---|\n" +
	"| `docs/ci.md` | A pipeline job fails |\n" +
	"| `docs/work/<work>.md` | Handoff and next steps for one piece of work. Index: `docs/work/README.md` |\n"

// A project router that names a tracker wins over the default file, and the
// guard lets the tracker's markdown files and the router through.
func TestHandoffProjectRouter(t *testing.T) {
	dir := guardEnv(t)
	repo := filepath.Join(dir, "repo")
	writeFile(t, filepath.Join(repo, ".git", "HEAD"), "ref: refs/heads/main\n")
	writeFile(t, filepath.Join(repo, "CLAUDE.md"), trackerRouter)
	cwd := filepath.Join(repo, "sub")
	os.MkdirAll(cwd, 0o755)

	out, _ := runGuard(t, fxHigh5, payloadFor("Bash", cwd, ""))
	for _, want := range []string{"router doc", "Handoff and next steps for one piece of work",
		"one file per piece of work", "References row", "Do not commit or stage the handoff"} {
		if !strings.Contains(out, want) {
			t.Errorf("the deny has no %q: %s", want, out)
		}
	}
	if strings.Contains(out, "HANDOFF.md") {
		t.Errorf("the default file came before the router: %s", out)
	}

	tracker := filepath.Join(repo, "docs", "work", "login-fix.md")
	cases := []struct {
		tool, file string
		pass       bool
	}{
		{"Write", tracker, true},
		{"Edit", filepath.Join(repo, "docs", "work", "README.md"), true},
		{"Read", filepath.Join(repo, "CLAUDE.md"), true},
		{"Edit", filepath.Join(repo, "CLAUDE.md"), true},
		{"Write", filepath.Join(repo, "docs", "work", "run.sh"), false},
		{"Write", filepath.Join(repo, "docs", "ci.md"), false},
		{"Write", filepath.Join(cwd, "HANDOFF.md"), false},
		{"Bash", tracker, false},
	}
	for _, c := range cases {
		out, _ := runGuard(t, fxHigh5, payloadFor(c.tool, cwd, c.file))
		if (out == "") != c.pass {
			t.Errorf("%s %s: pass=%v, got %s", c.tool, c.file, c.pass, out)
		}
	}

	// The handoff is kept out of git, and the router is not.
	raw, _ := os.ReadFile(filepath.Join(repo, ".git", "info", "exclude"))
	ex := string(raw)
	if strings.Count(ex, "/docs/work/login-fix.md\n") != 1 {
		t.Errorf("the handoff is not excluded once:\n%s", ex)
	}
	if strings.Contains(ex, "CLAUDE.md") {
		t.Errorf("the router was excluded:\n%s", ex)
	}
	runGuard(t, fxHigh5, payloadFor("Write", cwd, tracker))
	raw, _ = os.ReadFile(filepath.Join(repo, ".git", "info", "exclude"))
	if strings.Count(string(raw), "/docs/work/login-fix.md\n") != 1 {
		t.Errorf("a second write added the entry again:\n%s", raw)
	}
}

// A router in the user's Claude folder applies when the project has none,
// and a project router comes before it.
func TestHandoffUserRouter(t *testing.T) {
	dir := guardEnv(t)
	claude := filepath.Join(dir, "claude")
	writeFile(t, filepath.Join(claude, "CLAUDE.md"),
		"- Next steps for a piece of work go in `work/{project}/{work}.md`, indexed in `work/{project}/index.md`.\n")
	cwd := filepath.Join(dir, "proj")
	os.MkdirAll(cwd, 0o755)

	out, _ := runGuard(t, fxHigh5, payloadFor("Bash", cwd, ""))
	if !strings.Contains(out, "Next steps for a piece of work go in") {
		t.Fatalf("the user router was not used: %s", out)
	}
	if out, _ := runGuard(t, fxHigh5, payloadFor("Write", cwd, filepath.Join(claude, "work", "proj", "fix.md"))); out != "" {
		t.Errorf("the tracker file was denied: %s", out)
	}

	writeFile(t, filepath.Join(cwd, "AGENTS.md"), "Write the handoff to `notes/next.md`.\n")
	out, _ = runGuard(t, fxHigh5, payloadFor("Bash", cwd, ""))
	if !strings.Contains(out, "Write the handoff to") || strings.Contains(out, "Next steps for a piece of work go in") {
		t.Fatalf("the project router did not come first: %s", out)
	}
	if out, _ := runGuard(t, fxHigh5, payloadFor("Write", cwd, filepath.Join(claude, "work", "proj", "fix.md"))); out == "" {
		t.Error("the user tracker still passed under a project rule")
	}
	if out, _ := runGuard(t, fxHigh5, payloadFor("Write", cwd, filepath.Join(cwd, "notes", "next.md"))); out != "" {
		t.Errorf("the project file was denied: %s", out)
	}
}

// A handoff row with no path, or a mention with no backticked path, does not
// count, so the default file stays.
func TestHandoffRouterNeedsAPath(t *testing.T) {
	dir := guardEnv(t)
	cwd := filepath.Join(dir, "proj")
	writeFile(t, filepath.Join(cwd, "CLAUDE.md"), "Keep a handoff up to date.\nNext steps: run `go test ./...` and `make`.\n")
	out, _ := runGuard(t, fxHigh5, payloadFor("Bash", cwd, ""))
	if !strings.Contains(out, "one file only") || !strings.Contains(out, "HANDOFF.md") {
		t.Fatalf("the default file was not used: %s", out)
	}
}

// The default file is excluded in a worktree too, through the common git dir.
func TestHandoffExcludeWorktree(t *testing.T) {
	dir := guardEnv(t)
	common := filepath.Join(dir, "main", ".git")
	wt := filepath.Join(common, "worktrees", "wt")
	writeFile(t, filepath.Join(wt, "commondir"), "../..\n")
	cwd := filepath.Join(dir, "wt")
	writeFile(t, filepath.Join(cwd, ".git"), "gitdir: "+wt+"\n")

	if out, _ := runGuard(t, fxHigh5, payloadFor("Write", cwd, filepath.Join(cwd, "HANDOFF.md"))); out != "" {
		t.Fatalf("the handoff was denied: %s", out)
	}
	raw, err := os.ReadFile(filepath.Join(common, "info", "exclude"))
	if err != nil || !strings.Contains(string(raw), "/HANDOFF.md\n") {
		t.Fatalf("not excluded in the common git dir: %q %v", raw, err)
	}
}
