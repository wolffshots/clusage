# clusage

A terminal UI (Go + Bubble Tea) for watching your **Claude Code rate limit
windows**. It shows how much of each limit you have used, when each one
resets, and how the usage moved over the last hours or days.

```
  Now    History    Config    cron 30 4,10,16 * * 1-4

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

## Install

Go 1.25 or later is required.

```sh
go install github.com/wolffshots/clusage@latest
```

That puts the binary in `$(go env GOPATH)/bin`. To build from a source checkout
instead:

```sh
go build -o clusage .
```

macOS only for now. The token is stored with the `security` command, which does
not exist on Linux or Windows. Set `CLAUDE_CODE_OAUTH_TOKEN` in the environment
to skip the keychain on those platforms.

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
clusage           # open the TUI
clusage tui       # the same thing, named explicitly
clusage usage     # print one line per window and exit
```

The TUI opens on the last stored reading, so it shows numbers before it calls
the API. Press `r` for a fresh reading.

### Keys

| Key | Action |
|---|---|
| `1` `2` `3` | Now, History, Config tab |
| `r` | Fetch a reading now |
| `a` | Pause or resume the scheduled fetch |
| `tab` | Select the next limit window |
| `s` | Cycle the history span (6h, 24h, 7d, 30d) |
| `?` | Toggle the full help |
| `q` | Quit |

### Tabs

**Now** draws a gauge per limit window with its status and reset time. The
color tracks load: green under 60%, amber under 85%, red at or above 85%.

**History** graphs the selected window over the chosen span, with a sparkline
per window underneath for comparison. The scale is fixed at 0 to 100% rather
than autoscaled, because a week that sat between 40% and 42% would otherwise
render as a crisis.

**Config** shows the effective settings, whether the schedule parses, and when
the next scheduled fetch lands.

### One-shot output

`clusage usage` prints the same numbers as plain text and exits.

```sh
clusage usage             # uses the cached reading if it is fresh enough
clusage usage -force      # ignore the cache and call the API
clusage usage -verbose    # also print every rate limit header
clusage usage -model claude-sonnet-5
clusage usage -threshold 15
```

Flag defaults come from `config.json`, so a flag is only needed to override the
configured value for one run.

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
```

`TestRenderTabs` drives the model through `Update` and logs each tab, so
`go test -run TestRenderTabs -v .` prints the whole UI without a terminal.

## Credits

Styling follows [wolffshots/fftui](https://github.com/wolffshots/fftui). The
cron matcher started life in a sibling project.

## License

[MIT](LICENSE)
