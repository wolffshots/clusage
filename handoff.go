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
	"fmt"
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
	// routers are the router docs this session loads, found or not. A write
	// to one passes only when it adds a handoff row and changes nothing else,
	// because a team repository shares them.
	routers []string
	// local, user and shared are the routers a deny offers to take a new
	// handoff row: the project's personal CLAUDE.local.md, the one in the
	// user's Claude folder, and the one the team shares.
	local, user, shared string
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
	top := cwd
	if cwd != "" {
		// Claude Code loads the routers from the working directory up to the
		// repository root, so the handoff rule is looked for in each of them.
		root := repoRoot(cwd)
		for d := cwd; ; d = filepath.Dir(d) {
			dirs = append(dirs, d)
			top = d
			if root == "" || d == root || filepath.Dir(d) == d {
				break
			}
		}
	}
	user := ""
	if dir, err := claudeDir(); err == nil {
		dirs = append(dirs, dir)
		user = filepath.Join(dir, "CLAUDE.md")
	}
	var routers []string
	for _, d := range dirs {
		for _, name := range routerNames {
			routers = append(routers, filepath.Join(d, name))
		}
	}
	var p *handoffPlan
	for _, r := range routers {
		if p = routerRule(r); p != nil {
			break
		}
	}
	if p == nil {
		if cwd == "" && !filepath.IsAbs(setting) {
			return nil
		}
		if !filepath.IsAbs(setting) {
			setting = filepath.Join(cwd, setting)
		}
		p = &handoffPlan{file: filepath.Clean(setting)}
	}
	p.routers, p.user = routers, user
	if top != "" {
		p.local = filepath.Join(top, "CLAUDE.local.md")
		p.shared = filepath.Join(top, "CLAUDE.md")
		for _, name := range []string{"CLAUDE.md", "AGENTS.md", filepath.Join(".claude", "CLAUDE.md")} {
			if fileExists(filepath.Join(top, name)) {
				p.shared = filepath.Join(top, name)
				break
			}
		}
	}
	return p
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
		target := rowTarget(line)
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

// rowTarget is the handoff path a router line names, or "" when the line is
// not a handoff row. A handoff is markdown, or a tracker directory. A command
// such as `go test ./...` on a next steps line is neither.
func rowTarget(line string) string {
	if !handoffRow.MatchString(line) {
		return ""
	}
	for _, m := range handoffPath.FindAllStringSubmatch(line, -1) {
		if strings.HasSuffix(strings.ToLower(m[1]), ".md") || strings.HasSuffix(m[1], "/") {
			return m[1]
		}
	}
	return ""
}

// hasRow reports whether text holds a handoff row.
func hasRow(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		if rowTarget(line) != "" {
			return true
		}
	}
	return false
}

// rowText is a router line as one sentence: table pipes and list marks gone,
// and short enough for a deny.
func rowText(line string) string {
	s := strings.Join(strings.Fields(strings.NewReplacer("|", " ", "\t", " ").Replace(line)), " ")
	s = strings.TrimLeft(s, "-* ")
	r := []rune(s)
	return string(r[:min(len(r), 300)])
}

// toolInput is the part of a file tool's input the guard reads.
type toolInput struct {
	FilePath  string `json:"file_path"`
	Content   string `json:"content"`
	OldString string `json:"old_string"`
	NewString string `json:"new_string"`
	Edits     []struct {
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
	} `json:"edits"`
}

// allows reports whether a file tool may touch path. A router may be read,
// and written only to add a handoff row.
func (p *handoffPlan) allows(tool, path string, in toolInput) bool {
	path = filepath.Clean(path)
	if p.isRouter(path) {
		return tool == "Read" || addsRow(tool, path, in)
	}
	switch {
	case p.file != "" && samePath(path, p.file):
		return true
	case p.tree != "" && strings.EqualFold(filepath.Ext(path), ".md"):
		rel, err := filepath.Rel(p.tree, path)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	return false
}

// isRouter reports whether path is one of the session's router docs.
func (p *handoffPlan) isRouter(path string) bool {
	for _, r := range p.routers {
		if samePath(path, r) {
			return true
		}
	}
	return false
}

// addsRow reports whether a write to a router keeps everything in it and adds
// a handoff row. Anything else could rewrite a file the team shares.
func addsRow(tool, path string, in toolInput) bool {
	added := func(old, updated string) (string, bool) {
		if !strings.Contains(updated, old) {
			return "", false
		}
		return strings.Replace(updated, old, "", 1), true
	}
	switch tool {
	case "Write":
		raw, _ := os.ReadFile(path)
		a, ok := added(string(raw), in.Content)
		return ok && hasRow(a)
	case "Edit":
		if in.OldString == "" {
			return false
		}
		a, ok := added(in.OldString, in.NewString)
		return ok && hasRow(a)
	case "MultiEdit":
		row := false
		for _, e := range in.Edits {
			if e.OldString == "" {
				return false
			}
			a, ok := added(e.OldString, e.NewString)
			if !ok {
				return false
			}
			row = row || hasRow(a)
		}
		return row
	}
	return false
}

// excludes reports whether a write to path goes into .git/info/exclude. A
// router the team shares never does. CLAUDE.local.md is personal, so it does.
func (p *handoffPlan) excludes(path string) bool {
	return !p.isRouter(path) || strings.EqualFold(filepath.Base(path), "CLAUDE.local.md")
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
			b.WriteString("The guard still lets Read, Write and Edit through for markdown files under " + p.tree + ", and for nothing else. ")
		} else {
			b.WriteString("The guard still lets Read, Write and Edit through for " + p.file + ", and for nothing else. ")
		}
		b.WriteString("Read the router first, and do not change it. ")
	} else {
		b.WriteString(p.askWhere())
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

// askWhere is the deny text when no router names a handoff. The agent must
// not add a row to a router on its own, because a team repository shares
// those files, so it asks the user where the handoff goes.
func (p *handoffPlan) askWhere() string {
	var opts []string
	opts = append(opts, p.file+", the default: local to this directory, kept out of git, and no router changes")
	if p.local != "" {
		opts = append(opts, "a tracker for this project only: add a handoff row to "+p.local+", which is personal and kept out of git")
	}
	if p.user != "" {
		opts = append(opts, "a tracker for every project: add a handoff row to "+p.user+", such as | `work/{project}/{work}.md` | Handoff and next steps for one piece of work. Index: `work/{project}/README.md` |")
	}
	if p.shared != "" {
		opts = append(opts, "a tracker the team shares: add a handoff row to "+p.shared+", which a push shares with the team")
	}
	var b strings.Builder
	b.WriteString("No router doc (CLAUDE.md, CLAUDE.local.md, .claude/CLAUDE.md or AGENTS.md) in this project or in the user's Claude folder says where handoffs go. Do not add that to any router doc on your own, because a team repository may share those files. ")
	b.WriteString("Before you write it, ask one more short multiple choice question, where the handoff goes, and wait for the answer. The options: ")
	for i, o := range opts {
		fmt.Fprintf(&b, "(%d) %s. ", i+1, o)
	}
	b.WriteString("If the user names another place, that is a handoff row too: add it to ")
	if p.local != "" {
		b.WriteString(p.local + " for a place inside this project, or to ")
	}
	b.WriteString(p.user + " for a place outside it. ")
	b.WriteString("A handoff row is one line that says handoff or next steps and gives the path in backticks, a markdown file or a directory ending in /. A placeholder such as <work> makes it a file per piece of work, for example | `.claude/work/<work>.md` | Handoff and next steps for one piece of work. Index: `.claude/work/README.md` |. A path is relative to the router it is in. Put the row in the router's References table if it has one. Add only that row, and change nothing else in the router, because the guard lets through only an edit that adds a handoff row. The guard reads the row on the next call and then lets the files it names through. ")
	b.WriteString("For the default, the guard lets Read, Write and Edit through for " + p.file + ". For a row, write the handoff where the row says, one file per piece of work, and add a row for it to the index. ")
	return b.String()
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
