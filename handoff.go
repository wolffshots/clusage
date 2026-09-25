package main

// The handoff. A deny at a cut offers to write the session's state to a file,
// so a fresh session can resume from it. The guard then lets the file tools
// through for that file and nothing else.
//
// A project can say where its handoffs go. A router doc, the always-loaded
// CLAUDE.md or AGENTS.md that router-reference-docs lays out, names its work
// trackers in a References row. A row that mentions a handoff or next steps
// wins over the handoff_file setting, so each piece of work keeps its own
// file under the project's index. The project's routers come first, then the
// one in the user's Claude folder.
//
// A handoff is scratch state for the next session, not project history, so
// the guard keeps it out of git: it adds the file to the repository's
// .git/info/exclude, which is local and never committed.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// handoffPlan is where the handoff goes for one session.
type handoffPlan struct {
	// file is the one file the handoff goes to, "" when a router names a
	// tracker tree instead.
	file string
	// router is the router doc that configures the handoff, "" for the
	// handoff_file setting. rule is its row, and target the path it names,
	// resolved, with any placeholder kept.
	router, rule, target string
	// tree is the directory a router's tracker lives in. Any markdown file
	// under it may be written, so each piece of work gets its own file and
	// the index beside them stays writable.
	tree string
}

// routerNames are the files Claude Code loads into every session, so a
// handoff rule written into one of them is a rule the project already follows.
var routerNames = []string{"CLAUDE.md", "CLAUDE.local.md", filepath.Join(".claude", "CLAUDE.md"), "AGENTS.md"}

// handoffRow matches a row that sets where handoffs go, and handoffPath the
// backticked path in it.
var (
	handoffRow  = regexp.MustCompile(`(?i)hand-?offs?\b|next[ -]steps`)
	handoffPath = regexp.MustCompile("`([^`\\s]+)`")
	placeholder = regexp.MustCompile(`<[^>]*>|\{[^}]*\}`)
)

// planHandoff resolves the handoff for a session in cwd. It returns nil when
// the setting is off.
func planHandoff(setting, cwd string) *handoffPlan {
	setting = strings.TrimSpace(setting)
	if setting == "" || strings.EqualFold(setting, "off") {
		return nil
	}
	if cwd == "" {
		if wd, err := os.Getwd(); err == nil {
			cwd = wd
		}
	}
	var dirs []string
	if cwd != "" {
		// Claude Code loads the routers from the working directory up to the
		// repository root, so the handoff rule is looked for in each of them.
		root := repoRoot(cwd)
		for d := cwd; ; d = filepath.Dir(d) {
			dirs = append(dirs, d)
			if root == "" || d == root || filepath.Dir(d) == d {
				break
			}
		}
	}
	if dir, err := claudeDir(); err == nil {
		dirs = append(dirs, dir)
	}
	for _, d := range dirs {
		for _, name := range routerNames {
			if p := routerRule(filepath.Join(d, name)); p != nil {
				return p
			}
		}
	}
	if cwd == "" && !filepath.IsAbs(setting) {
		return nil
	}
	if !filepath.IsAbs(setting) {
		setting = filepath.Join(cwd, setting)
	}
	return &handoffPlan{file: filepath.Clean(setting)}
}

// routerRule reads a router doc and returns the handoff it configures, or nil.
// A path is relative to the router's directory, and "~/" is the home
// directory. A placeholder such as <work> or {slug} marks a file per piece of
// work, so the directory above the first one becomes the tracker tree.
func routerRule(router string) *handoffPlan {
	raw, err := os.ReadFile(router)
	if err != nil {
		return nil
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !handoffRow.MatchString(line) {
			continue
		}
		target := ""
		for _, m := range handoffPath.FindAllStringSubmatch(line, -1) {
			// A handoff is markdown, or a tracker directory. A command such
			// as `go test ./...` on a next steps line is neither.
			if strings.HasSuffix(strings.ToLower(m[1]), ".md") || strings.HasSuffix(m[1], "/") {
				target = m[1]
				break
			}
		}
		if target == "" {
			continue
		}
		dirForm := strings.HasSuffix(target, "/")
		if rest, ok := strings.CutPrefix(target, "~/"); ok {
			home, err := os.UserHomeDir()
			if err != nil {
				continue
			}
			target = filepath.Join(home, rest)
		} else if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(router), target)
		}
		p := &handoffPlan{router: router, rule: rowText(line), target: target}
		if loc := placeholder.FindStringIndex(target); loc != nil {
			p.tree = filepath.Dir(target[:loc[0]] + "x")
		} else if dirForm {
			p.tree = filepath.Clean(target)
		} else {
			p.file = filepath.Clean(target)
		}
		return p
	}
	return nil
}

// rowText is a router line as one sentence: table pipes and list marks gone,
// and short enough for a deny.
func rowText(line string) string {
	s := strings.Join(strings.Fields(strings.NewReplacer("|", " ", "\t", " ").Replace(line)), " ")
	s = strings.TrimLeft(s, "-* ")
	r := []rune(s)
	return string(r[:min(len(r), 300)])
}

// allows reports whether a file tool may touch path. The router itself is
// writable, because it may hold the index the new handoff goes into.
func (p *handoffPlan) allows(path string) bool {
	path = filepath.Clean(path)
	switch {
	case p.file != "" && samePath(path, p.file):
		return true
	case p.router != "" && samePath(path, p.router):
		return true
	case p.tree != "" && strings.EqualFold(filepath.Ext(path), ".md"):
		rel, err := filepath.Rel(p.tree, path)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	return false
}

// samePath compares two cleaned paths. Windows file names ignore case.
func samePath(a, b string) bool {
	if filepath.Separator == '\\' {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// steps says how to write the handoff. Every other tool stays denied, so the
// agent writes from what the session already knows.
func (p *handoffPlan) steps() string {
	var b strings.Builder
	b.WriteString("If the user picks the handoff, only the main agent writes it. ")
	if p.router != "" {
		b.WriteString("This project keeps its handoffs where its router doc " + p.router + " says, and that rule comes before any default: \"" + p.rule + "\". ")
		if p.tree != "" {
			b.WriteString("Write one file per piece of work at " + p.target + ", named for this piece of work. Reuse its file if one exists. Then add or update its row in the index the router names, as a trigger that says what the file holds and when to open it, the way router-reference-docs writes a References row. ")
			b.WriteString("The guard still lets Read, Write and Edit through for markdown files under " + p.tree + " and for " + p.router + ", and for nothing else. ")
		} else {
			b.WriteString("The guard still lets Read, Write and Edit through for " + p.file + " and for " + p.router + ", and for nothing else. ")
		}
		b.WriteString("Read the router first. ")
	} else {
		b.WriteString("The guard still lets Read, Write and Edit through for one file only: " + p.file + ". ")
	}
	b.WriteString("Call no other tool, and do not run git, a build or a test to gather facts, because those calls are denied. Write from what this session already knows. If the file exists, Read it first, then replace it with Write. Write it as markdown for an agent that starts with no context, with these sections: the goal, in the user's own words; what is done, naming the files changed and whether the changes are committed and pushed, and on which branch; the work in progress and exactly where it stopped; the next steps, as an ordered checklist; decisions made and why, and dead ends not to retry; open questions for the user; the commands that build and test the work. Keep it short, well under 200 lines. ")
	b.WriteString("Do not commit or stage the handoff. It is local state for the next session. Inside a repository the guard adds it to .git/info/exclude, so git does not offer it. ")
	b.WriteString("Then tell the user the path, and that a fresh session resumes with: read <that path> and continue from its next steps. Then end the turn.")
	path := p.file
	if path == "" {
		path = "the file you wrote"
	}
	return strings.ReplaceAll(b.String(), "<that path>", path)
}

// repoRoot is the directory above dir that holds .git, or "".
func repoRoot(dir string) string {
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		if filepath.Dir(d) == d {
			return ""
		}
	}
}

// gitDir is the directory that holds info/exclude for the repository at
// root. A worktree or a submodule has a .git file that points at its git
// directory, and a worktree shares its exclude file with the main checkout.
func gitDir(root string) string {
	dot := filepath.Join(root, ".git")
	fi, err := os.Stat(dot)
	if err != nil {
		return ""
	}
	if fi.IsDir() {
		return dot
	}
	raw, err := os.ReadFile(dot)
	if err != nil {
		return ""
	}
	dir, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), "gitdir:")
	if !ok {
		return ""
	}
	dir = strings.TrimSpace(dir)
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(root, dir)
	}
	if c, err := os.ReadFile(filepath.Join(dir, "commondir")); err == nil {
		common := strings.TrimSpace(string(c))
		if !filepath.IsAbs(common) {
			common = filepath.Join(dir, common)
		}
		return filepath.Clean(common)
	}
	return dir
}

// excludeHandoff adds path to the local exclude file of the repository that
// holds it, so a handoff never shows up as a file to commit. The exclude file
// is never committed itself. A tracked file stays tracked, because an exclude
// only hides untracked files. Errors are dropped: the handoff matters more
// than the entry.
func excludeHandoff(path string) {
	root := repoRoot(filepath.Dir(path))
	if root == "" {
		return
	}
	dir := gitDir(root)
	if dir == "" {
		return
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(rel, "..") || strings.HasPrefix(filepath.ToSlash(rel), ".git/") {
		return
	}
	entry := "/" + filepath.ToSlash(rel)
	exclude := filepath.Join(dir, "info", "exclude")
	raw, _ := os.ReadFile(exclude)
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(l) == entry {
			return
		}
	}
	if err := os.MkdirAll(filepath.Dir(exclude), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(exclude, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		f.WriteString("\n")
	}
	f.WriteString("# clusage handoff, local state for the next session\n" + entry + "\n")
}
