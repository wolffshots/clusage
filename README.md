# clusage

A terminal UI (Go + Bubble Tea) for watching your **Claude Code rate limit
windows**. It shows how much of each limit you have used, when each one
resets, and how the usage moved over the last hours or days.

```
  Now    History    Tokens    Config    cron 30 4,10,16 * * 1-4

▸ 5h        █████████████████████████████████████████████████████████░░░░░  92%
    ● allowed_warning   resets Mon 19:19 (in 2h0m)

  7d        █████████████████████████░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░  41%
    ● allowed   resets Thu 15:19 (in 70h0m)

  7d-opus   ███████████████████████████████████████░░░░░░░░░░░░░░░░░░░░░░░  63%
    ● allowed

read 20m0s ago  ·  claude-opus-5
```

## How it works

The Anthropic API reports your remaining budget in `anthropic-ratelimit-*`
response headers. There is no endpoint that returns them on their own, so
clusage makes the smallest possible inference call (one input token, one output
token) and reads the headers off the response.

Every reading goes into SQLite, which is what the history graphs draw from.

Each probe call also reports what it cost, so clusage records its own token
spend. The Tokens tab shows that spend, so the price of the monitoring is
visible next to the limits it monitors.

## Install

### Homebrew (macOS / Linux)

```sh
brew install wolffshots/tap/clusage
```

Builds from source (Go is installed as a build-only dependency) via
[wolffshots/homebrew-tap](https://github.com/wolffshots/homebrew-tap), so it
works on Intel and Apple silicon Macs and on Linux, with no Gatekeeper
quarantine step.

### Prebuilt binaries

Download the binary for your platform from the [latest release](https://github.com/wolffshots/clusage/releases/latest):

| Platform | Asset |
|---|---|
| Linux x86-64 | `clusage_<version>_linux_amd64` |
| Windows x86-64 | `clusage_<version>_windows_amd64.exe` |
| macOS (Apple silicon) | `clusage_<version>_darwin_arm64` |

On Linux/macOS, make it executable and check it runs:

```sh
chmod +x clusage_*          # the file you downloaded
./clusage_* --version
```

macOS binaries are unsigned, so the first launch is blocked by Gatekeeper.
Right-click then **Open**, or clear the quarantine flag with
`xattr -d com.apple.quarantine clusage_*_darwin_arm64`. To verify a download,
run `sha256sum -c checksums.txt` (Linux) or `shasum -a 256 -c checksums.txt`
(macOS).

### From source

Go 1.25 or later is required.

```sh
go install github.com/wolffshots/clusage@latest
```

That puts the binary in `$(go env GOPATH)/bin`. From a checkout, `go build -o
clusage .` builds it in place.

Both report `clusage dev` rather than a version number. The version is injected
by the release build, and `go install` passes no linker flags. Use Homebrew or a
release binary if you want `--version` to name the release.

### Platform support

The keychain path is macOS only. `clusage setup` shells out to the `security`
command, which does not exist on Linux or Windows. Set
`CLAUDE_CODE_OAUTH_TOKEN` in the environment to skip the keychain on those
platforms. Everything else works on all three.

## Setup

Store your Claude Code OAuth token once:

```sh
clusage setup
```

The token goes into the login keychain under the service name `clusage`. The
prompt hides what you type. `CLAUDE_CODE_OAUTH_TOKEN` takes priority over the
keychain when it is set.

## Run

```sh
clusage             # open the TUI
clusage tui         # the same thing, named explicitly
clusage usage       # print one line per window and exit
clusage --version   # print the version and exit
```

The TUI opens on the last stored reading, so it shows numbers before it calls
the API. Press `r` for a fresh reading.

### Keys

| Key | Action |
|---|---|
| `1` `2` `3` `4` | Now, History, Tokens, Config tab |
| `r` | Fetch a reading now |
| `a` | Pause or resume the scheduled fetch |
| `tab` | Select the next limit window |
| `s` | Cycle the history and token span (6h, 24h, 7d, 30d) |
| `?` | Toggle the full help |
| `q` | Quit |

### Tabs

**Now** draws a gauge per limit window with its status and reset time. The
color tracks load: green under 60%, amber under 85%, red at or above 85%.

**History** graphs the selected window over the chosen span, with a sparkline
per window underneath for comparison. The scale is fixed at 0 to 100% rather
than autoscaled, because a week that sat between 40% and 42% would otherwise
render as a crisis.

**Tokens** graphs what clusage spent on its own probe calls: a cumulative
total over the chosen span, a per-call sparkline, and the breakdown into input,
output, cache write and cache read. The all-time total is not limited by the
span.

The cache rows read 0 in normal use. Prompt caching needs a prefix of about
1024 tokens, and a probe call is around 15 tokens, so nothing upstream will
cache it. The rows are recorded anyway, because they come from the response
`usage` block and cost nothing to keep. There is no cache header on the
response; the body is the only place these counts appear.

**Config** shows the effective settings, whether the schedule parses, and when
the next scheduled fetch lands.

### One-shot output

`clusage usage` prints the same numbers as plain text and exits.

```sh
clusage usage             # uses the cached reading if it is fresh enough
clusage usage -force      # ignore the cache and call the API
clusage usage -verbose    # also print every header and the token cost
clusage usage -model claude-sonnet-5
clusage usage -threshold 15
```

Flag defaults come from `config.json`, so a flag is only needed to override the
configured value for one run.

## Guard rail hook

`hooks/clusage-guard.sh` is a Claude Code hook. On `PreToolUse` it reads the
same numbers as `clusage usage` and acts on them before each tool call:

| Condition | Action |
|---|---|
| A 5h or 7d window is exhausted | Deny the call at once. Overage is paying, so a retry only spends more. |
| 5h window at or above 90% | Pause the tool call. Poll every 15s. Release the call once the window drops below 90%. |
| 5h window still high after 45s | Deny the call, and tell the agent when to retry. |
| Any 7d window at or above 95% | Deny the call at once. No polling. |
| The tool is a scheduling tool | Allow the call, so the agent can book its retry. |
| `clusage` missing or failing | Allow the call. |

A deny names the window, its percent, and its reset clock time. It then tells
the agent to set a timer or a wake-up for that time and to retry then, without
running `clusage` again to check. Where the window reports no reset time, the
deny tells the agent to stop and report to the user instead.

The exhausted case is different. A window that reports a status other than
`allowed` has spent its quota, so the next call comes out of overage. The guard
denies at once, with no wait and no retry advice, because waiting does not help
and a retry costs real money. Set `CLUSAGE_GUARD_ALLOW_OVERAGE=1` to opt in to
working on overage.

The guard reads the `PreToolUse` event on stdin and lets the scheduling tools
through, so a denied agent can still book the retry it was just told to make.
`CLUSAGE_GUARD_ALLOW_TOOLS` holds that list.

The hook runs in front of every agent and subagent, so one session cannot talk
its way past the limit. A pause is a sleep inside the hook, so the agent spends
no tokens while it waits.

Keep that pause short. Claude Code decides a session is dead when a hook blocks
for minutes, and kills it with "the session stopped responding". So the hook
waits out a short spike, then denies the call and hands the decision back. The
next message the user sends is checked again, and work continues once the
window drops. Raise `CLUSAGE_GUARD_MAXWAIT` only if a longer block is safe on
your client.

```sh
clusage hook install      # register it in ~/.claude/settings.json
clusage hook status       # show the registered command and timeout
clusage hook uninstall    # remove it again
```

Install does two things. It links the script that ships with this build into
`~/.claude/hooks/clusage-guard.sh`, and it writes one `PreToolUse` entry and one
`SessionStart` entry naming that link. The rest of `settings.json` stays as it
is. An install over an older version adds the `SessionStart` entry and leaves
everything else alone.

The registered path is therefore the same on every machine, whatever the
install prefix is. A `brew upgrade` replaces the script the link points at, so
the hook upgrades with clusage and needs no further step. `clusage hook status`
prints the link and its target, and reports a broken link.

Install refuses to overwrite a real file at the link path. Uninstall removes the
entry and the link, and never a real file. Set `CLAUDE_CONFIG_DIR` to work on a
different settings file.

Checks are at most one per 6 minutes, and a cached check costs about 20ms.

### Guard rail settings

Every threshold is an environment variable, so no config file is needed:

| Variable | Default | Meaning |
|---|---|---|
| `CLUSAGE_GUARD_DISABLE` | `0` | Set to `1` to turn the guard off. |
| `CLUSAGE_GUARD_5H` | `90` | Soft threshold, percent. Pause and poll. |
| `CLUSAGE_GUARD_7D` | `95` | Hard threshold, percent. Deny without polling. |
| `CLUSAGE_GUARD_INTERVAL` | `360` | Seconds between checks while under the soft threshold. |
| `CLUSAGE_GUARD_POLL` | `15` | Seconds between checks while paused. |
| `CLUSAGE_GUARD_MAXWAIT` | `45` | Deny after pausing this long. |
| `CLUSAGE_GUARD_ALLOW_OVERAGE` | `0` | Set to `1` to keep working once a window is exhausted. |
| `CLUSAGE_GUARD_ALLOW_TOOLS` | `ScheduleWakeup CronCreate` | Tool names that pass without a check. |

### The off switch

The guard also stops if `~/.claude/clusage-guard.off` exists:

```sh
touch ~/.claude/clusage-guard.off    # guard off
rm ~/.claude/clusage-guard.off       # guard on again
```

`CLUSAGE_GUARD_DISABLE` cannot help here. The desktop app gives no way to set
an environment variable for one session, so a tripped guard would deny every
tool call, including the calls needed to fix the guard. A file works from
inside the session. `clusage hook status` reports the switch when it is there.

Keep `CLUSAGE_GUARD_MAXWAIT` under the hook timeout in `settings.json`. Install
sets that timeout 15 seconds above the maximum wait. A hook that times out lets
the tool call through.

A poll reads a new probe every time, so a pause costs one small API call per
`CLUSAGE_GUARD_POLL` seconds.

## Stale resume report

The same script also runs on `SessionStart`, for a resumed or forked session. A
prompt cache lives about an hour. Resume after that and the whole conversation
is re-sent and cached again, at full price, before the first reply.

Claude Code measures that gap and hands the hook four numbers, so the report
costs no probe and no API call. When the cache has expired, the hook prints one
line:

```
clusage: prompt cache expired after 90m idle. This session re-sends 182k tokens, about $1.14. Consider /compact.
```

The hook stays silent while the cache is still warm, and on a Claude Code older
than v2.1.251, which does not send the numbers.

The line goes out on two channels. A terminal shows the `systemMessage`. The
Claude desktop app runs Claude Code with `--output-format stream-json`, where
that message goes to the SDK stream instead of the screen, so the hook also
hands the line to Claude as `additionalContext` and asks it to open with the
warning.

Compacting cannot save that cache. Nothing can, because the cost is already
sunk once the gap has passed. `/compact` shrinks what the *next* hour re-sends.
Set `CLUSAGE_RESUME_DISABLE=1` to turn the report off.

## Config

Clusage writes `config.json` on first run, at `~/.config/clusage/config.json`
(or under `XDG_CONFIG_HOME` when that is set). The SQLite file sits beside it as
`clusage.db`.

```json
{
  "model": "claude-opus-5",
  "threshold_minutes": 5,
  "fetch_cron": "*/15 * * * *",
  "history_hours": 168
}
```

| Field | Meaning |
|---|---|
| `model` | Which model to ping. Cheaper models report the same headers. |
| `threshold_minutes` | How long `clusage usage` reuses a cached reading. |
| `fetch_cron` | Schedule for the automatic fetch. Empty disables it. |
| `history_hours` | How far back the history graphs may read. |

Edit the file and restart to pick up a change.

## Scheduled fetches

`fetch_cron` takes a standard 5-field cron expression:

```
minute  hour  day-of-month  month  day-of-week
```

Each field accepts `*`, a number, a range (`9-17`), a list (`4,10,16`), and a
step (`*/15`, `9-17/2`). Day-of-week runs 0 to 6 from Sunday, and accepts 7 for
Sunday as well.

Separate several expressions with `;`. One expression cannot always cover a
schedule, so this is how you get a fetch at 09:05 and 18:35 but not at 09:35.

| `fetch_cron` value | Fires |
|---|---|
| `*/15 * * * *` | Every 15 minutes |
| `0 9-17 * * 1-5` | Hourly, weekday work hours |
| `30 4,10,16 * * 1-4` | 04:30, 10:30 and 16:30, Monday to Thursday |
| `5 9 * * *;35 18 * * *` | 09:05 and 18:35 |
| An empty string | Never. Auto-fetch is off |

The tab bar shows the active schedule, and marks the last scheduled fetch with
`✓` or `✗`. A scheduled fetch that fails leaves the last good reading on
screen, so the display never blanks while you are away from it.

Two limits worth knowing:

- **The schedule only runs while the TUI is open.** Nothing accumulates in the
  background, so history is sparse until clusage has been open a few times.
- **`day-of-month` and `day-of-week` are ANDed.** Standard cron ORs them when
  both are restricted. Avoid restricting both in one expression.

An expression that does not parse disables the auto-fetch instead of firing at
the wrong time. The Config tab flags it in red.

## Development

```sh
go test ./...     # unit tests, plus a full render of every tab at 96x32
go vet ./...
bash hooks/clusage-guard.test.sh   # hook decisions, resume report, registration
```

`TestRenderTabs` drives the model through `Update` and logs each tab, so
`go test -run TestRenderTabs -v .` prints the whole UI without a terminal.

### Releasing

Pushing a `v*` tag runs [`.github/workflows/release.yml`](.github/workflows/release.yml),
which tests, cross-builds for Linux, Windows and macOS, and creates a GitHub
release with the binaries and a `checksums.txt`.

```sh
git tag -a v0.1.1 -m "clusage v0.1.1"
git push origin v0.1.1
```

The build injects the tag into `main.version` with
`-ldflags "-X main.version=<tag>"`, so `clusage --version` reports it.

Then bump the Homebrew formula in
[wolffshots/homebrew-tap](https://github.com/wolffshots/homebrew-tap). Point
`url` at the new tag and update `sha256`:

```sh
curl -fL https://github.com/wolffshots/clusage/archive/refs/tags/v0.1.1.tar.gz | shasum -a 256
```

Two traps in that one command:

- **Keep `-f`.** Without it, curl prints the 404 body and exits 0, so you hash
  GitHub's error page. The result is 64 valid hex characters, and `brew style`
  accepts it. The formula then fails for every user.
- **Hash that exact URL.** `gh api repos/OWNER/REPO/tarball/TAG` returns a
  different byte stream with a different hash, so a hash taken from it will not
  match what Homebrew downloads.

Verify before pushing the tap, which catches both:

```sh
brew style Formula/clusage.rb
brew install --build-from-source wolffshots/tap/clusage
brew test wolffshots/tap/clusage
```

## Credits

Styling follows [wolffshots/fftui](https://github.com/wolffshots/fftui). The
cron matcher started life in a sibling project.

## License

[MIT](LICENSE)
