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
clusage makes the smallest possible inference call and reads the headers off the
response.

The call asks for no reply at all. It sends `max_tokens: 0`, which runs prefill
and returns an empty content block with the headers intact. Output costs five
times what input does, so a probe that generates nothing is the cheapest one
that still carries the numbers.

If the API ever stops accepting that shape, it answers 400. Only on a 400,
clusage sends the call again asking for one token, which is the shape that
worked before. A 429 or a 5xx means the shape was fine and the call was refused,
so there is no second probe there.

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

**Now** also shows `burn 14.2%/h  full in 4h43m` under each gauge. The
projection targets 100 percent, because the question a usage viewer answers is
when the window is spent. The guard rail keeps its own thresholds, in the
`guard` section of the config, so the two never disagree by accident. A window with too little
history reads `burn -`.

The Now tab reads the burn rate from the history span you selected with `s`,
which starts at 24h. `clusage usage` always reads 7 days. A short span holds
fewer readings than the 7d window smooths over, so the Now tab can show a
coarser 7d rate than `clusage usage`, or `burn -` where the command prints a
number. Widen the span to compare the two.

**History** graphs the selected window over the chosen span, with a sparkline
per window underneath for comparison. The scale is fixed at 0 to 100% rather
than autoscaled, because a week that sat between 40% and 42% would otherwise
render as a crisis.

**History** graphs the burn rate under the utilization chart, on its own
`%/h` scale, because a rate has no natural ceiling. A short terminal drops the
rate chart to a single sparkline row, and a very short one drops it entirely,
so the help footer always stays visible.

**Tokens** graphs what clusage spent on its own probe calls: a cumulative
total over the chosen span, a per-call sparkline, and the breakdown into input,
output, cache write and cache read. The all-time total is not limited by the
span.

The output row reads 0, because the probe asks for no reply. A count above 0
means the 400 fallback ran. The cache rows read 0 too, in normal use.
The shortest prefix that prompt caching will hold is model dependent. It is
512 tokens on `claude-opus-5` and 4096 on `claude-haiku-4-5`. A probe call is
around 23 tokens, so no model caches it. The rows are recorded anyway,
because they come from the response `usage` block and cost nothing to keep.
There is no cache header on the response; the body is the only place these
counts appear.

**Config** shows the effective settings, whether the schedule parses, and when
the next scheduled fetch lands. It also reports the guard rail thresholds the
hook would apply, and marks a row an environment variable overrode. Last come
the config and database paths, whether a token is stored, and the version.

### One-shot output

`clusage usage` prints the same numbers as plain text and exits.

```sh
clusage usage             # uses the cached reading if it is fresh enough
clusage usage -force      # ignore the cache and call the API
clusage usage -verbose    # also print every header and the token cost
clusage usage -model claude-sonnet-5
clusage usage -threshold 15
```

The table carries one row per window:

```text
5h       61% used   allowed           14.2%/h   resets Wed 19:30 (in 4h36m)
7d       41% used   allowed           0.9%/h    resets Mon 18:00 (in 123h6m)
overage  0% used    allowed
```

The `%/h` column is the burn rate: percent of that window consumed per hour. A
rate of `100%/h` empties the window in one hour. It comes from the stored
utilization history, not from the token counts, because the `calls` table only
records what clusage's own probe calls cost.

The column is blank when the history cannot support an estimate. That covers a
fresh database, the minutes right after a window resets, and a history whose
newest reading is too old to speak for now.

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
| `clusage` reports no usable window | Deny the call at once. No polling. |

A deny names the window, its percent, and its reset clock time. It then tells
the agent to set a timer or a wake-up for that time and to retry then, without
running `clusage` again to check. Where the window reports no reset time, the
deny tells the agent to stop and report to the user instead.

The last row denies when the guard cannot read a number, rather than assuming
there is room. That covers a missing
`clusage`, a probe that fails, and a table that parses but carries no 5h or no
7d row. A partial table is the dangerous case: a missing 7d row leaves the hard
cut unenforced, which is the one thing this guard exists to prevent.

A denied agent cannot repair `clusage`, so that deny carries its own way out. It
gives the user two commands, `clusage usage -force` to see the underlying error
and `touch ~/.claude/clusage-guard.off` to stand the guard down. The off switch
matters more here than anywhere else, because a broken probe otherwise denies
every tool call, including the ones needed to diagnose it.

A deny never parks the agent on its own. It asks you first, and it only sets a
wake-up if you say to wait. A wait longer than 55 minutes is chained into legs,
because a wake-up caps at one hour and a longer gap expires the prompt cache.
Each interim leg schedules the next one and does nothing else, so a long wait
costs almost no tokens.

Pick the pay-overage option and the guard has to stand down, which the agent
cannot do for you. Every tool it would use to make the off switch is denied. So
the deny hands the agent the two commands to give you:

```sh
touch ~/.claude/clusage-guard.off    # stand the guard down, work on overage
rm ~/.claude/clusage-guard.off       # put it back when the work is done
```

The guard stays off while that file exists, so the second command matters. An
exhausted window prints the same pair, next to the `CLUSAGE_GUARD_ALLOW_OVERAGE`
variable, which only a terminal session can set.

The exhausted case is different. A window that reports a status other than
`allowed` has spent its quota, so the next call comes out of overage. The guard
denies at once, with no wait and no retry advice, because waiting does not help
and a retry costs real money. Set `CLUSAGE_GUARD_ALLOW_OVERAGE=1` to opt in to
working on overage.

The guard reads the `PreToolUse` event on stdin and lets the scheduling tools
through, so a denied agent can still book the retry it was just told to make.
`CLUSAGE_GUARD_ALLOW_TOOLS` holds that list.

The list has to cover both things a deny asks for. A deny tells the agent to put
the choice to the user as a multiple choice question, and then to book a retry,
so `AskUserQuestion` is on the list beside the two wake-up tools. Leave it there.
Drop it and the guard denies the very question its own deny message demands, and
the agent falls back to asking in plain text.

Every deny also names the list, because a denied agent cannot find out what it
may still call except by trying, and a try costs a denial. The names come from
the live value of `CLUSAGE_GUARD_ALLOW_TOOLS`, so a custom list cannot drift out
of step with the message.

An empty `CLUSAGE_GUARD_ALLOW_TOOLS` does not clear the list. An empty value
reads as unset, so the default comes back. Name the tools you want instead.

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
clusage guard-config      # print the thresholds the script reads
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

How often a check runs scales with usage. The guard remembers what the last
check saw and picks the next wait from it, on a quadratic ramp between
`CLUSAGE_GUARD_INTERVAL` and `CLUSAGE_GUARD_INTERVAL_MIN`. Each window counts
against its own threshold, and the closer of the two drives the wait:

| 5h | 7d | Next check |
|---|---|---|
| 10% | 5% | 297s |
| 50% | 20% | 217s |
| 80% | 40% | 87s |
| 89% | 20% | 36s |
| 90% | 20% | 30s |
| 20% | 90% | 58s |

The ramp stays slow while there is headroom, because every check spends real
usage. It falls to 30s near a threshold, where overshooting into overage costs
more than the probes do. Run `bash clusage-guard.sh --interval <5h> <7d>` to
print the wait for any pair. Set both bounds to the same number for a fixed
interval. A cached check costs about 20ms.

Every bound tolerates a bad value rather than break the guard.
`CLUSAGE_GUARD_INTERVAL`, `CLUSAGE_GUARD_INTERVAL_MIN`, `CLUSAGE_GUARD_POLL`
and `CLUSAGE_GUARD_MAXWAIT` each fall back to their default unless the value
reads as a whole number. A floor above the ceiling is treated as a typo, and
the ceiling wins. A percent that is missing or unreadable counts as no load,
which gives the longest wait.

Zero stays meaningful for three of them. A ceiling of 0 means check on every
tool call, which is a probe per call. A floor of 0 lets the ramp reach zero. A
wait of 0 denies at once. `CLUSAGE_GUARD_POLL` is the exception and takes a
floor of one second, because `sleep 0` never advances the wait and the pause
loop would never end.

The ramp is the floor, not the whole rule. When the table carries a burn rate,
the guard also projects when each window reaches its own threshold, and takes
whichever wait is shorter:

    project = (threshold - percent) / rate * 3600 / 4

The division by four lands four checks before the threshold rather than one.
An unknown or falling rate contributes nothing, so the ramp decides on its own.

Run `bash clusage-guard.sh --project <percent> <rate> <cut>` to print the wait
for any triple.

### Guard rail settings

Every setting has a field in the `guard` section of `config.json` and an
environment variable. The variable wins, so a single terminal session can
override the machine. A value neither of them supplies falls back to the
default in the table.

| `guard` field | Variable | Default | Meaning |
|---|---|---|---|
| `soft_5h_percent` | `CLUSAGE_GUARD_5H` | `90` | Soft threshold, percent. Pause and poll. |
| `hard_7d_percent` | `CLUSAGE_GUARD_7D` | `95` | Hard threshold, percent. Deny without polling. |
| `interval_seconds` | `CLUSAGE_GUARD_INTERVAL` | `300` | Seconds between checks at low usage. |
| `interval_min_seconds` | `CLUSAGE_GUARD_INTERVAL_MIN` | `30` | Seconds between checks at a threshold. |
| `poll_seconds` | `CLUSAGE_GUARD_POLL` | `15` | Seconds between checks while paused. |
| `max_wait_seconds` | `CLUSAGE_GUARD_MAXWAIT` | `45` | Deny after pausing this long. |
| `allow_overage` | `CLUSAGE_GUARD_ALLOW_OVERAGE` | `false` | Set to `1` to keep working once a window is exhausted. |
| `allow_tools` | `CLUSAGE_GUARD_ALLOW_TOOLS` | `ScheduleWakeup CronCreate AskUserQuestion` | Tool names that pass without a check. |

The switches below have no config field. Two of them turn a feature off, which
is what the off switch file below is for, and two exist for the test suite.

| Variable | Default | Meaning |
|---|---|---|
| `CLUSAGE_GUARD_DISABLE` | `0` | Set to `1` to turn the guard off. |
| `CLUSAGE_RESUME_DISABLE` | `0` | Set to `1` to turn the resume report off. |
| `CLUSAGE_GUARD_STATE` | `$TMPDIR/clusage-guard-$USER.stamp` | Where the last check is recorded. Every session shares one file. |
| `CLUSAGE_GUARD_FIXTURE` | unset | Read usage from a file instead of clusage. Also skips the config read. |

The hook is a shell script, so it cannot parse JSON. It reads the fields
through `clusage guard-config`, which prints them as `key=value` lines:

```sh
$ clusage guard-config
soft_5h=90
hard_7d=95
interval=300
interval_min=30
poll=15
maxwait=45
allow_overage=0
allow_tools=ScheduleWakeup CronCreate AskUserQuestion
```

The script reads those lines key by key and never evaluates them, and it takes
only a known key with a plain value. A clusage that is missing or broken yields
no lines, which leaves the defaults. The Config tab reports the resolved
numbers, and marks the rows an environment variable took over.

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

The file names every field, so nothing is hidden behind a default:

```json
{
  "model": "claude-opus-5",
  "threshold_minutes": 5,
  "fetch_cron": "*/15 * * * *",
  "history_hours": 168,
  "guard": {
    "soft_5h_percent": 90,
    "hard_7d_percent": 95,
    "interval_seconds": 300,
    "interval_min_seconds": 30,
    "poll_seconds": 15,
    "max_wait_seconds": 45,
    "allow_overage": false,
    "allow_tools": [
      "ScheduleWakeup",
      "CronCreate",
      "AskUserQuestion"
    ]
  }
}
```

| Field | Meaning |
|---|---|
| `model` | Which model to ping. Cheaper models report the same headers. |
| `threshold_minutes` | How long `clusage usage` reuses a cached reading. |
| `fetch_cron` | Schedule for the automatic fetch. Empty disables it. |
| `history_hours` | How far back the history graphs may read. |
| `guard` | The guard rail hook's thresholds. See [Guard rail settings](#guard-rail-settings). |

A field you delete falls back to its default. A value out of range does the
same, so a typo never disables the guard.

Edit the file and restart to pick up a change. The Config tab reports the
resolved settings, the paths, and whether the hook is registered.

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

The hook tests never call the API. `CLUSAGE_GUARD_FIXTURE` names a file holding
a `clusage usage` table, and the guard reads that file instead of running
`clusage`. Use it to replay any usage state by hand.

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
