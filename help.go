package main

import (
	"fmt"
	"slices"
	"strings"
)

const mainHelp = `clusage shows how much of your Claude subscription limits you have used.

Usage:
  clusage [command] [flags]

Commands:
  tui           Open the dashboard. This is the default with no command.
  usage         Print one line per limit window and exit.
  statusline    Store the numbers Claude Code gives its status line.
  setup         Store a token in the macOS keychain.
  hook          Run, install, remove or check the guard rail hook.
  guard         Turn the guard rail off or back on.
  guard-config  Print the guard rail settings the hook applies.
  doctor        Print a diagnosis of the setup, with suggestions.
  help          Show this help, or the help for one command.

Flags:
  -h, --help     Show help. After a command, show that command's help.
  --version      Print the version and exit.

Getting started:
  1. Pick a source, and set "source" in ~/.config/clusage/config.json:
       statusline  Recommended. Reads what Claude Code shows in its status
                   line. No token, no API call. Needs a running session.
       usage       Reads the account usage endpoint. Needs a Claude Code
                   login or a token.
       probe       Sends one tiny API call and reads its rate limit headers.
                   Needs a Claude Code login or a token.
       auto        Tries usage, then a recent statusline reading, then probe.
     Set "fallback" to a second source to try when the first fails, such as
     "source": "usage" with "fallback": "probe".
  2. For statusline, see: clusage help statusline
     For usage, probe or auto, log in with claude. See: clusage help setup
  3. Run clusage usage, or clusage for the dashboard.

Files:
  ~/.config/clusage/config.json  settings, created on the first run
  ~/.config/clusage/clusage.db   readings and token history
  XDG_CONFIG_HOME moves both.

Run clusage help <command> for details. The README covers everything else.
`

var commandHelp = map[string]string{
	"tui": `Usage:
  clusage
  clusage tui

Opens the dashboard on the last stored reading, so numbers show before any
fetch. Press r to fetch now and ? to list every key.

Tabs:
  1 now      each window, its percent, burn rate and reset time
  2 history  percent over time, tab picks the window, s the span
  3 tokens   what clusage spent on its own probe calls
  4 config   the settings in use, the token and the hook status
  5 diagnostics  suggestions, tokens, calls, errors and build details.
             Shown only when "diagnostics": true is set in config.json.
             Scroll it with the arrow keys, pgup, pgdn, home and end.

The dashboard also fetches on the "fetch_cron" schedule in config.json. The
"probe_cron" schedule sends a probe call whatever the source is, which starts
a 5h window. Press a to pause or resume both.

The tab bar counts the reads that failed in the last 24 hours, from every
clusage run. The config tab breaks the count down by source and status.
`,

	"usage": `Usage:
  clusage usage [-force] [-verbose] [-source name] [-threshold minutes] [-model name]

Prints one line per limit window: the name, the percent used, the status, the
burn rate and when the window resets. The last line says whether the reading
is live or cached, how old it is, and where it came from.

It reuses the last reading while it is newer than threshold_minutes, so
running it often is cheap. Otherwise it reads the configured source.

Flags:
  -force              Ignore the cached reading and read the source now.
  -verbose            Also print every stored header, and the token cost of a
                      probe call.
  -source name        The source to read. Default: "source" in config.json.
                      "fallback" still applies.
  -threshold minutes  How old a cached reading may be. Default:
                      threshold_minutes in config.json, 5 if unset.
  -model name         The model a probe call uses. Default: "model" in
                      config.json, claude-haiku-4-5 if unset.

Examples:
  clusage usage
  clusage usage -force
  clusage usage -threshold 15
  clusage usage -source probe -force   # from cron, to start a 5h window

If it fails:
  "no source set"  Set "source" in config.json. See: clusage help
  "no token"       See: clusage help setup
  "statusline: no reading yet"
                   Claude Code has not run clusage statusline yet. See:
                   clusage help statusline
  usage: ... 429   The usage endpoint limits how often it answers. clusage
                   waits before it calls it again: the retry-after time, or
                   one minute that doubles per 429, up to 15 minutes.
                   Set "fallback": "probe" so the probe answers meanwhile.
  "the API rejected the OAuth token (401 Unauthorized)"
                   Run claude to log in again. If you set a token yourself,
                   replace it with a new one. See: clusage help setup
`,

	"statusline": `Usage:
  clusage statusline < session.json

Claude Code runs this command, not you. It sends the session JSON on stdin,
and the command prints the windows, such as "5h 23% · 7d 41%", for the status
bar. It stores a reading only when the numbers change.

Setup:
  1. Add this to ~/.claude/settings.json:
       "statusLine": { "type": "command", "command": "clusage statusline" }
     Use the full path to clusage if it is not on the PATH Claude Code sees.
  2. Set "source": "statusline" (or "auto") in ~/.config/clusage/config.json.
  3. Restart Claude Code and send one message.
  4. Check it: clusage usage -force

Limits:
  - Needs Claude Code v2.1.80 or later and a Pro or Max plan.
  - The numbers come from that session's own API responses. Use in Claude
    Desktop or another session shows up only after this session makes a call.
  - It reports the 5h and 7d windows only, not Opus-only or overage.
  - Run by hand from a terminal, it exits with a hint instead of waiting.
`,

	"setup": `Usage:
  clusage setup

The usage, probe and auto sources need an OAuth token. clusage looks in this
order, and stops at the first it finds:

  1. CLAUDE_CODE_OAUTH_TOKEN in the environment.
  2. The clusage keychain entry that this command writes (macOS).
  3. The Claude Code login:
       macOS          the "Claude Code-credentials" keychain entry
       Linux/Windows  ~/.claude/.credentials.json, or CLAUDE_CONFIG_DIR

Once claude has logged in, step 3 needs no setup at all. This command is only
for storing a separate token on macOS:

  claude setup-token   # prints a token, needs a Claude subscription
  clusage setup        # paste it; the prompt hides what you type

If it fails:
  "the Claude Code login has expired"
      clusage never refreshes the login, because a refresh would log Claude
      Code out. Run claude once, then try again.
  "no token found"
      Log in with claude, or set CLAUDE_CODE_OAUTH_TOKEN.
  "no token has the user:profile scope the usage endpoint needs"
      A token from claude setup-token can probe but cannot read the usage
      endpoint. Log in with claude, and clusage tries that login next. Or
      use the probe source.

A token that the API refuses, or that lacks the scope the usage endpoint
needs, gives way to the next token in the list above.
`,

	"hook": `Usage:
  clusage hook install
  clusage hook status
  clusage hook uninstall
  clusage hook run
  clusage hook interval <5h percent> <7d percent>
  clusage hook project <percent> <rate> <cut>

The guard rail hook runs before each Claude Code tool call and checks your
limits the way clusage usage does. It pauses when the 5h window is nearly
spent, and denies calls once a window is exhausted or the 7d window passes its
cut. A deny tells the agent when to retry. On a resumed session it reports
what the resume costs once the prompt cache has expired. It runs on macOS,
Linux and Windows.

Actions:
  install    Register clusage hook run in ~/.claude/settings.json. The rest of
             settings.json is untouched. An entry for the old hook script is
             rewritten, and its link in ~/.claude/hooks is removed.
  status     Show the registered command, its timeout, and the off switch.
  uninstall  Remove the registration again.
  run        Handle one hook event. Claude Code runs this, with the event on
             stdin.
  interval   Print the seconds the guard waits between checks at these levels.
  project    Print the wait the burn rate projection picks, if any.

Install writes "clusage hook run" when clusage is on PATH. Otherwise it names
this binary by its full path, in the exec form that runs with no shell.

Turn it off for a while:
  clusage guard off   # guard stands down in every session
  clusage guard on    # guard is back
Or press o on the Now tab of the TUI.

The cuts and timings live under "guard" in config.json. See:
clusage help guard-config
`,

	"guard": `Usage:
  clusage guard off
  clusage guard on
  clusage guard status

Turns the guard rail off for every Claude Code session, or back on. Off creates
~/.claude/clusage-guard.off, and on removes it. A denied agent cannot run this
itself, so a deny names these commands for you to run.
`,

	"guard-config": `Usage:
  clusage guard-config

Prints the guard rail settings as key=value lines, after the config file and
the environment. Use it to check what the hook will apply.

Keys, from "guard" in ~/.config/clusage/config.json:
  soft_5h        pause at this 5h percent (soft_5h_percent, default 90)
  hard_7d        deny past this 7d percent (hard_7d_percent, default 95)
  interval       seconds between checks at low usage (default 300)
  interval_min   seconds between checks near a cut (default 30)
  poll           seconds between checks while paused (default 15)
  maxwait        seconds to pause before a deny (default 45)
  allow_overage  1 keeps working once a window is exhausted (default 0)
  allow_tools    tools that always pass
  handoff_file   the file a denied agent may write its state to, for a
                 fresh session to resume from (default HANDOFF.md, off
                 drops the option)

A CLUSAGE_GUARD_* environment variable overrides the file for one session.
Values out of range fall back to the default.
`,

	"doctor": `Usage:
  clusage doctor

Prints what the Diagnostics tab shows, as plain text at full width. Paste it
into a bug report. It never prints a token, only a fingerprint: the first 16
hex digits of a SHA-256 of it.

Sections:
  Suggestions        what to fix, most urgent first
  Setup              the source chain, 429 waits, last readings, status line,
                     guard hook and schedules
  Tokens             every place a token can live, its expiry and scopes, and
                     which token the usage and probe sources send
  Calls              the last good and failed call per source, latency, and
                     the most recent calls with their request-id
  Errors             failed reads of the last 24 hours
  Latest reading     windows in local time, UTC and epoch, the stored headers,
                     and the clock skew against the API
  Account and build  subscription, rate limit tier, version and commit
  Database and guard table sizes and the guard rail hook's last check

It reads the keychain, so macOS can ask to allow it once.
`,

	"help": `Usage:
  clusage help [command]

Shows the overview, or the help for one command.
`,
}

// isHelpFlag reports whether arg asks for help.
func isHelpFlag(arg string) bool {
	return arg == "-h" || arg == "--help" || arg == "-help"
}

// printHelp prints the overview, or one command's help when topic names one.
func printHelp(topic string) error {
	if topic == "" {
		fmt.Print(mainHelp)
		return nil
	}
	text, ok := commandHelp[topic]
	if !ok {
		return fmt.Errorf("no help for %q: want one of %s", topic, strings.Join(commandNames(), ", "))
	}
	fmt.Print(text)
	return nil
}

func commandNames() []string {
	var names []string
	for name := range commandHelp {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
