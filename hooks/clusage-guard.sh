#!/usr/bin/env bash
# clusage guard rail: a Claude Code hook. It handles two events.
#
# On PreToolUse it pauses tool calls while the 5h rate limit window sits at or
# above a soft threshold, and denies them once a 7d window passes a hard
# threshold. The numbers come from `clusage usage`.
#
# On SessionStart it reports what a resumed session costs to re-send, but only
# once the prompt cache behind it has expired. Claude Code measures that and
# passes it in, so this path calls nothing and adds no probe.
#
# Register it with `clusage hook install`, or run this script directly with
# --install. Install links the script into the Claude Code hooks directory and
# names that link in settings.json, so an upgrade of clusage upgrades the hook.
# Run --status to see the current state, --uninstall to remove it. Run
# --interval <5h percent> <7d percent> to print the wait the ramp picks. Run
# --project <percent> <rate> <cut> to print the wait the projection picks.
#
# Config (environment):
#   CLUSAGE_GUARD_DISABLE=1     turn the guard off
#   CLUSAGE_RESUME_DISABLE=1    turn the resume report off
#   CLUSAGE_GUARD_5H=90         soft threshold, percent, pause and poll
#   CLUSAGE_GUARD_7D=95         hard threshold, percent, deny without polling
#   CLUSAGE_GUARD_INTERVAL=300  seconds between checks at low usage
#   CLUSAGE_GUARD_INTERVAL_MIN=30  seconds between checks near a threshold
#   CLUSAGE_GUARD_POLL=15       seconds between checks while paused
#   CLUSAGE_GUARD_MAXWAIT=45    deny after pausing this long
#   CLUSAGE_GUARD_ALLOW_OVERAGE=1  work on even when a window is exhausted
#   CLUSAGE_GUARD_ALLOW_TOOLS="A B"  tool names that pass without a check
#   CLUSAGE_GUARD_FIXTURE=path  read usage text from a file instead of clusage
#
# Touch $CLAUDE_DIR/clusage-guard.off to turn the guard off for every session.
set -uo pipefail

SOFT=${CLUSAGE_GUARD_5H:-90}
HARD=${CLUSAGE_GUARD_7D:-95}
INTERVAL=${CLUSAGE_GUARD_INTERVAL:-300}
INTERVAL_MIN=${CLUSAGE_GUARD_INTERVAL_MIN:-30}
POLL=${CLUSAGE_GUARD_POLL:-15}
# A hook that blocks for minutes makes the Claude Code session look dead, and
# the app kills it. Wait only for a short spike, then hand the decision back.
MAXWAIT=${CLUSAGE_GUARD_MAXWAIT:-45}
ALLOW_TOOLS=${CLUSAGE_GUARD_ALLOW_TOOLS:-"ScheduleWakeup CronCreate"}
STATE="${TMPDIR:-/tmp}/clusage-guard-${USER:-x}.stamp"
# Absolute, but deliberately unresolved. The caller may name this script
# through a stable path such as <brew prefix>/share/clusage/hooks, and
# resolving it would bury a version number in the link that --install makes.
SELF=${BASH_SOURCE[0]}
[[ "$SELF" != /* ]] && SELF="$PWD/$SELF"
CLAUDE_DIR=${CLAUDE_CONFIG_DIR:-$HOME/.claude}
LINK="$CLAUDE_DIR/hooks/clusage-guard.sh"
OFF="$CLAUDE_DIR/clusage-guard.off"

# --- the poll interval ------------------------------------------------------

# interval_for <5h percent> <7d percent>. Seconds to wait before the next
# check. Each window is measured against its own cut, and the closer of the two
# drives the wait. The ramp is quadratic, so it stays near INTERVAL while there
# is headroom and falls to INTERVAL_MIN at the cut. Every probe spends real
# usage, so a slow ramp at low usage is the point.
#
# A percent that is missing, negative or not a number reads as no load, which
# gives the longest wait.
#
# ponytail: a percent is a level, not a rate. Burn rate over the stored
# readings would give the honest wait of headroom divided by burn. Upgrade to
# that once clusage exposes a rate.
interval_for() {
  awk -v f="${1:--1}" -v v="${2:--1}" -v s="$SOFT" -v h="$HARD" \
      -v hi="$INTERVAL" -v lo="$INTERVAL_MIN" 'BEGIN {
    if (hi + 0 <= 0) { print 0; exit }   # zero means check every call
    if (lo + 0 < 0) lo = 0
    if (lo + 0 > hi + 0) lo = hi         # a floor above the ceiling is a typo
    a = (s + 0 > 0) ? (f + 0) / s : 0
    b = (h + 0 > 0) ? (v + 0) / h : 0
    x = (a > b) ? a : b
    if (x < 0) x = 0
    if (x > 1) x = 1
    n = int(hi - (hi - lo) * x * x + 0.5)
    print (n < 1) ? 1 : n
  }'
}

# project <percent> <rate> <cut>. Seconds until the window reaches cut at this
# rate, divided by four so four checks land before it rather than one. Prints
# nothing when the rate is unknown or not positive, or when the cut is already
# behind, because there is then nothing to project.
project() {
  awk -v p="${1:--1}" -v r="${2:-0}" -v c="${3:-0}" 'BEGIN {
    if (r + 0 <= 0) exit
    if (p + 0 < 0) exit
    if (c + 0 <= p + 0) exit
    n = int((c - p) / r * 3600 / 4 + 0.5)
    print (n < 1) ? 1 : n
  }'
}

# mark <5h percent> <7d percent> <5h rate> <7d rate>. Records when the last
# check ran and what it saw, so the next call sizes its own wait from the same
# numbers. An older stamp holds fewer fields, which read as unknown.
mark() {
  printf '%s %s %s %s %s\n' "$(date +%s)" \
    "${1:--1}" "${2:--1}" "${3:--1}" "${4:--1}" > "$STATE"
}

# --- registration -----------------------------------------------------------

# Points $LINK at this copy of the script. settings.json then names $LINK, so
# the registered path stays the same whatever the install prefix is, and an
# upgrade of clusage upgrades the hook.
link() {
  [[ "$SELF" == "$LINK" ]] && return 0   # running the link itself, leave it
  mkdir -p "$(dirname "$LINK")" || return 1
  if [[ -e "$LINK" && ! -L "$LINK" ]]; then
    echo "clusage-guard: $LINK is a real file, not a link. Move it away first." >&2
    return 1
  fi
  ln -sfn "$SELF" "$LINK"
}

# Removes $LINK, but never a real file that the user put there.
unlink_hook() {
  [[ -L "$LINK" ]] && rm -f "$LINK"
  return 0
}

# Edits settings.json in place. Python keeps the existing key order, so the
# rest of the file stays as the user wrote it.
manage() {
  command -v python3 >/dev/null || {
    echo "clusage-guard: python3 is needed to edit settings.json" >&2
    return 1
  }
  python3 - "$1" "$CLAUDE_DIR/settings.json" "$LINK" "$((MAXWAIT + 15))" <<'PY'
import collections, json, os, sys

mode, path, script, timeout = sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4])
command = "bash '%s'" % script
settings = collections.OrderedDict()
if os.path.exists(path):
    with open(path) as fh:
        settings = json.load(fh, object_pairs_hook=collections.OrderedDict)

# One event per row: the event name, its matcher, and the hook timeout. The
# guard rail needs the whole pause budget. The resume report only reads the
# payload it is handed, so it needs seconds.
EVENTS = [("PreToolUse", "*", timeout), ("SessionStart", "resume|fork", 10)]

hooks = settings.setdefault("hooks", collections.OrderedDict())


# owned <entries>. The entries of one event that this script registered.
def owned(entries):
    return [e for e in entries
            if any("clusage-guard" in h.get("command", "") for h in e.get("hooks", []))]


if mode == "status":
    found = False
    for event, _, _ in EVENTS:
        for entry in owned(hooks.get(event, [])):
            for hook in entry["hooks"]:
                if "clusage-guard" in hook.get("command", ""):
                    found = True
                    print("registered:", event, hook["command"],
                          "(timeout %ss)" % hook.get("timeout", 60))
    if not found:
        print("not registered in", path)
        sys.exit(1)
    if os.path.islink(script):
        # The immediate target, not the fully resolved path. Homebrew points
        # <prefix>/share/clusage at the installed version, and resolving that
        # last hop would read as if the link named a version.
        target = os.readlink(script)
        print("link:", script, "->", target)
        if "/Cellar/" in target:
            print("warning: that target dies on the next upgrade,",
                  "rerun `clusage hook install`")
    if not os.path.exists(script):
        print("broken: nothing at", script)
        sys.exit(1)
    sys.exit(0)

if mode == "install":
    for event, matcher, event_timeout in EVENTS:
        entries = hooks.setdefault(event, [])
        mine = owned(entries)
        # An install over an older version rewrites the entry it already made,
        # so a new event is added and an existing one is brought up to date.
        for entry in mine:
            entry["matcher"] = matcher
            for hook in entry["hooks"]:
                if "clusage-guard" in hook.get("command", ""):
                    hook["command"], hook["timeout"] = command, event_timeout
        if not mine:
            entries.append(collections.OrderedDict([
                ("matcher", matcher),
                ("hooks", [collections.OrderedDict([
                    ("type", "command"), ("command", command),
                    ("timeout", event_timeout)])]),
            ]))
    action = "installed"
else:
    found = False
    for event, _, _ in EVENTS:
        entries = hooks.get(event, [])
        mine = owned(entries)
        found = found or bool(mine)
        keep = [e for e in entries if e not in mine]
        if keep:
            hooks[event] = keep
        elif event in hooks:
            del hooks[event]
    if not hooks:
        del settings["hooks"]
    action = "removed" if found else "was not registered"

os.makedirs(os.path.dirname(path), exist_ok=True)
with open(path, "w") as fh:
    json.dump(settings, fh, indent=2)
    fh.write("\n")
print("clusage guard rail %s in %s" % (action, path))
PY
}

case "${1:-}" in
  --install)   link && manage install; exit $? ;;
  --uninstall) manage uninstall && unlink_hook; exit $? ;;
  --status)    [[ -e "$OFF" ]] && echo "off switch present: $OFF"
               manage status; exit $? ;;
  --interval)  interval_for "${2:-}" "${3:-}"; exit 0 ;;
  --project)   project "${2:-}" "${3:-}" "${4:-}"; exit 0 ;;
  "") ;;
  *) echo "clusage-guard: unknown flag $1 (want: --install, --uninstall, --status, --interval, --project)" >&2; exit 1 ;;
esac

# --- the resume report ------------------------------------------------------

# report_resume <payload>. Prints the systemMessage for a stale resume, and
# returns non-zero when the payload is for another event, so the caller falls
# through to the guard rail.
report_resume() {
  command -v python3 >/dev/null || return 1
  python3 - "$1" <<'PY'
import json, os, sys

try:
    payload = json.loads(sys.argv[1])
except ValueError:
    sys.exit(1)
if payload.get("hook_event_name") != "SessionStart":
    sys.exit(1)
# The event is handled from here on, whether or not it prints anything. The
# guard rail must not run on a session start.
if os.environ.get("CLUSAGE_RESUME_DISABLE") == "1":
    sys.exit(0)
# Claude Code sends the four fields below on a resume or a fork, from v2.1.251.
# An older build, a fresh session, or a live cache all mean nothing to report.
if not payload.get("prompt_cache_likely_expired"):
    sys.exit(0)
try:
    minutes = int(payload["seconds_since_last_response"]) // 60
    tokens = int(payload["context_tokens"])
    usd = float(payload["estimated_cache_write_usd"])
except (KeyError, TypeError, ValueError):
    sys.exit(0)
size = "%.0fk" % (tokens / 1000.0) if tokens >= 1000 else str(tokens)
report = ("clusage: prompt cache expired after %dm idle. This session re-sends "
          "%s tokens, about $%.2f. Consider /compact." % (minutes, size, usd))
# Two channels, because no single one reaches every client. A terminal shows
# the systemMessage. The Claude desktop app runs Claude Code with
# --output-format stream-json, where the message goes to the SDK stream and
# never reaches the screen, so the same line is handed to Claude as context
# with an instruction to say it.
json.dump({"systemMessage": report,
           "hookSpecificOutput": {
               "hookEventName": "SessionStart",
               "additionalContext":
                   "%s Open this session by telling the user that line, in one "
                   "sentence, before anything else." % report}},
          sys.stdout)
print()
PY
}

# Claude Code writes the event payload to stdin. A hook run by hand has no
# payload, and reads nothing.
payload=""
if [[ ! -t 0 ]]; then
  IFS= read -r -d '' -t 2 payload
fi
if [[ -n "$payload" ]]; then
  report_resume "$payload" && exit 0
fi

# --- the guard rail ---------------------------------------------------------

[[ "${CLUSAGE_GUARD_DISABLE:-0}" == 1 ]] && exit 0
# The off switch. An environment variable cannot be set for one session in the
# desktop app, so a tripped guard would otherwise deny the work of fixing it.
[[ -e "$OFF" ]] && exit 0

deny() {
  local msg=${1//\\/}
  msg=${msg//\"/}
  printf '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"%s"}}\n' "$msg"
  exit 0
}

# read_usage <cache-minutes>. A short cache means each poll reads a new probe.
read_usage() {
  if [[ -n "${CLUSAGE_GUARD_FIXTURE:-}" ]]; then
    cat "$CLUSAGE_GUARD_FIXTURE"
  else
    clusage usage -threshold "$1" 2>/dev/null
  fi
}

# win <table> <window prefix>. "<percent>|<status>|<rate>|<reset>" for the
# highest matching window. The percent is -1 when nothing matched. The rate is
# the field ending in "%/h", found by scanning rather than by position, so an
# older clusage that prints no such field reads as an unknown rate. The reset
# is the text from the "resets" field to the end of the line, and stays last
# because it holds spaces. An exhausted status wins over the one on the highest
# row, because a sibling window such as 7d-opus can be spent at a low percent.
win() {
  awk -v p="$2" '$1 ~ "^"p && $2 ~ /%$/ {
    t = ($4 == "resets" ? "" : $4)
    if (x == "" && t != "" && t !~ /^allowed/) x = t
    if (m == "" || $2+0 > m) {
      m = $2+0; s = t; r = ""; b = ""
      for (i = 4; i <= NF; i++) if ($i ~ /%\/h$/) { b = $i; sub(/%\/h$/, "", b); break }
      for (i = 4; i <= NF; i++) if ($i == "resets") {
        for (j = i; j <= NF; j++) r = r (j > i ? " " : "") $j
        break
      }
    }
  } END { printf "%s|%s|%s|%s\n", (m == "" ? -1 : m), (x == "" ? s : x), b, r }' <<<"$1"
}

# ge <a> <b>. True when a is at least b. A percent can carry a fraction, and
# bash arithmetic reads that as a syntax error and then compares nothing.
ge() {
  awk -v a="$1" -v b="$2" 'BEGIN { exit !(a+0 >= b+0) }'
}

# spent <win>. True when the window reported a status that is not an allowed
# one. The window is then exhausted, so overage is paying for the next call.
spent() {
  local s=${1#*|}; s=${s%%|*}
  [[ -n "$s" && "$s" != allowed* ]]
}

# field <n> <pipe-separated string>. The nth field, one based.
field() {
  local IFS='|'
  local -a parts
  read -r -a parts <<<"$2"
  printf '%s' "${parts[$(($1 - 1))]:-}"
}

# check <cache-minutes>. One decision line:
#
#   verdict|window|percent|status|5h percent|7d percent|5h rate|7d rate|reset
#
# The verdict, the window it names and that window's own fields come first. The
# two raw percents and the two raw rates follow, because the caller sizes its
# next wait from all four, whichever window tripped. The reset text holds
# spaces, so it stays last for the reader to absorb. Nothing is printed when
# clusage is unavailable.
check() {
  local table five seven verdict=OK name=5h w pct status reset
  table=$(read_usage "$1")
  [[ -z "$table" ]] && return 0   # clusage unavailable, fail open
  five=$(win "$table" "5h")
  seven=$(win "$table" "7d")
  if [[ "${CLUSAGE_GUARD_ALLOW_OVERAGE:-0}" != 1 ]]; then
    if spent "$five"; then verdict=SPENT
    elif spent "$seven"; then verdict=SPENT name=7d
    fi
  fi
  if [[ "$verdict" == OK ]]; then
    if ge "${seven%%|*}" "$HARD"; then verdict=HARD name=7d
    elif ge "${five%%|*}" "$SOFT"; then verdict=SOFT
    fi
  fi
  w=$five
  [[ "$name" == 7d ]] && w=$seven
  IFS='|' read -r pct status _ reset <<<"$w"
  # verdict|window|percent|status|5h percent|7d percent|5h rate|7d rate|reset.
  # The two percents and the two rates drive the poll interval. The reset text
  # holds spaces, so it stays last for the reader to absorb.
  printf '%s|%s|%s|%s|%s|%s|%s|%s|%s\n' "$verdict" "$name" "$pct" "$status" \
    "${five%%|*}" "${seven%%|*}" "$(field 3 "$five")" "$(field 3 "$seven")" "$reset"
}

# trend <percent> <rate>. A clause naming the rate and when the window fills at
# it. Prints nothing when the rate is unknown or not positive, so a table from
# an older clusage produces the v0.8.0 message unchanged.
trend() {
  awk -v p="${1:--1}" -v r="${2:-0}" 'BEGIN {
    if (r + 0 <= 0) exit
    left = (100 - p) / r * 60
    if (left < 0) left = 0
    printf " It is rising at %.1f%%/h, so it fills in about %dm.", r, int(left + 0.5)
  }'
}

# retry <window> <reset>. Tells the caller when to come back.
retry() {
  if [[ -z "$2" ]]; then
    echo "The $1 window reported no reset time, so there is nothing to wait for. Stop all work now, in this agent and in every subagent, and report the limit to the user."
    return 0
  fi
  echo "It $2. Set a timer or a wake-up for that time and retry then. Trust that time. Do not run clusage again to check it. Stop all other work until then, in this agent and in every subagent."
}

# stop <window> <status>. An exhausted window means overage pays for the call.
stop() {
  deny "clusage guard rail: the $1 window is exhausted (status $2), so overage is paying for this call. Stop all work now, in this agent and in every subagent. Do not retry, because every retry spends more. Tell the user the window is exhausted and end the turn. Set CLUSAGE_GUARD_ALLOW_OVERAGE=1 to work on overage anyway."
}

# tool_name <payload>. The tool name from the PreToolUse event, so a scheduling
# tool can book the retry that a deny message just asked for. The payload is
# read once, further up. An empty payload reads as an unknown tool, and never
# blocks.
tool_name() {
  [[ -z "$1" ]] && return 0
  printf '%s' "$1" | python3 -c 'import json, sys
try:
    d = json.load(sys.stdin)
except Exception:
    d = {}
print(d.get("tool_name", "") if isinstance(d, dict) else "")' 2>/dev/null
}

now=$(date +%s)
last=0 pfive=-1 pseven=-1 rate5=-1 rate7=-1
if [[ -f "$STATE" ]]; then
  # "<unix time> <5h percent> <7d percent> <5h rate> <7d rate>". An older build
  # wrote fewer fields, and a truncated write leaves nothing usable, so each
  # field is taken only when it reads as one.
  ts="" p5="" p7="" b5="" b7=""
  read -r ts p5 p7 b5 b7 < "$STATE" 2>/dev/null
  [[ "$ts" =~ ^[0-9]+$ ]] && last=$ts
  [[ -n "$p5" ]] && pfive=$p5
  [[ -n "$p7" ]] && pseven=$p7
  [[ -n "$b5" ]] && rate5=$b5
  [[ -n "$b7" ]] && rate7=$b7
fi
interval=$(interval_for "$pfive" "$pseven")
# The level ramp is the floor. A projection that says the threshold is closer
# than the ramp thinks tightens the wait, and an unknown rate contributes
# nothing, so a degraded path behaves exactly as v0.8.0 does.
for cand in $(project "$pfive" "$rate5" "$SOFT") $(project "$pseven" "$rate7" "$HARD"); do
  # Floor the candidate, not the result. interval_for already honors its own
  # bounds, including the rule that a floor above the ceiling is a typo and the
  # ceiling wins. Flooring the result would override that and make the gate
  # wait longer than the ramp asked, which is the one thing this must not do.
  (( cand < INTERVAL_MIN )) && cand=$INTERVAL_MIN
  (( cand < interval )) && interval=$cand
done
(( now - last < interval )) && exit 0

tool=$(tool_name "$payload")
for allowed in $ALLOW_TOOLS; do
  [[ "$tool" == "$allowed" ]] && exit 0
done

# A probe older than the wait it just sized would report a stale number, so the
# reading cache tracks the interval. Under a minute this floors to zero, which
# is what a fast poll needs.
IFS='|' read -r verdict name value status pfive pseven rate5 rate7 reset <<<"$(check $((interval / 60)))"

if [[ "$verdict" == "SPENT" ]]; then
  stop "$name" "$status"
fi

if [[ "$verdict" == "HARD" ]]; then
  deny "clusage guard rail: the 7d limit is at ${value}% (hard cut at ${HARD}%).$(trend "$value" "$rate7") $(retry 7d "$reset")"
fi

if [[ "$verdict" == "SOFT" ]]; then
  waited=0
  while (( waited < MAXWAIT )); do
    sleep "$POLL"
    waited=$(( waited + POLL ))
    IFS='|' read -r verdict name value status pfive pseven rate5 rate7 reset <<<"$(check 0)"
    if [[ "$verdict" == "SPENT" ]]; then
      stop "$name" "$status"
    fi
    if [[ "$verdict" == "HARD" ]]; then
      deny "clusage guard rail: the 7d limit is at ${value}% (hard cut at ${HARD}%).$(trend "$value" "$rate7") $(retry 7d "$reset")"
    fi
    if [[ "$verdict" == "OK" ]]; then
      echo "clusage guard rail: 5h usage back down to ${value}%, work resumed after ${waited}s." >&2
      mark "$pfive" "$pseven" "$rate5" "$rate7"
      exit 0
    fi
  done
  deny "clusage guard rail: the 5h limit is at ${value}% and did not drop in ${MAXWAIT}s (soft limit ${SOFT}%).$(trend "$value" "$rate5") $(retry 5h "$reset")"
fi

mark "$pfive" "$pseven" "$rate5" "$rate7"
exit 0
