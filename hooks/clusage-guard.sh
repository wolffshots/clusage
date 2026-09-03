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
# Run --status to see the current state, --uninstall to remove it.
#
# Config (environment):
#   CLUSAGE_GUARD_DISABLE=1     turn the guard off
#   CLUSAGE_RESUME_DISABLE=1    turn the resume report off
#   CLUSAGE_GUARD_5H=90         soft threshold, percent, pause and poll
#   CLUSAGE_GUARD_7D=95         hard threshold, percent, deny without polling
#   CLUSAGE_GUARD_INTERVAL=360  seconds between checks while under threshold
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
INTERVAL=${CLUSAGE_GUARD_INTERVAL:-360}
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
  "") ;;
  *) echo "clusage-guard: unknown flag $1 (want: --install, --uninstall, --status)" >&2; exit 1 ;;
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

# win <table> <window prefix>. "<percent>|<status>|<reset>" for the highest
# matching window. The percent is -1 when nothing matched. The reset is the
# text from the "resets" field to the end of the line, and is empty when the
# window reported none.
win() {
  awk -v p="$2" '$1 ~ "^"p && $2 ~ /%$/ {
    if (m == "" || $2+0 > m) {
      m = $2+0; s = ($4 == "resets" ? "" : $4); r = ""
      for (i = 4; i <= NF; i++) if ($i == "resets") {
        for (j = i; j <= NF; j++) r = r (j > i ? " " : "") $j
        break
      }
    }
  } END { printf "%s|%s|%s\n", (m == "" ? -1 : m), s, r }' <<<"$1"
}

# spent <win>. True when the window reported a status that is not an allowed
# one. The window is then exhausted, so overage is paying for the next call.
spent() {
  local s=${1#*|}; s=${s%%|*}
  [[ -n "$s" && "$s" != allowed* ]]
}

check() {
  local table five seven
  table=$(read_usage "$1")
  [[ -z "$table" ]] && return 0   # clusage unavailable, fail open
  five=$(win "$table" "5h")
  seven=$(win "$table" "7d")
  if [[ "${CLUSAGE_GUARD_ALLOW_OVERAGE:-0}" != 1 ]]; then
    spent "$five" && { echo "SPENT|5h|$five"; return 0; }
    spent "$seven" && { echo "SPENT|7d|$seven"; return 0; }
  fi
  if (( ${seven%%|*} >= HARD )); then echo "HARD|7d|$seven"; return 0; fi
  if (( ${five%%|*} >= SOFT )); then echo "SOFT|5h|$five"; return 0; fi
  echo "OK|5h|$five"
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
last=0
[[ -f "$STATE" ]] && last=$(cat "$STATE" 2>/dev/null || echo 0)
(( now - last < INTERVAL )) && exit 0

tool=$(tool_name "$payload")
for allowed in $ALLOW_TOOLS; do
  [[ "$tool" == "$allowed" ]] && exit 0
done

IFS='|' read -r verdict name value status reset <<<"$(check 5)"

if [[ "$verdict" == "SPENT" ]]; then
  stop "$name" "$status"
fi

if [[ "$verdict" == "HARD" ]]; then
  deny "clusage guard rail: the 7d limit is at ${value}% (hard cut at ${HARD}%). $(retry 7d "$reset")"
fi

if [[ "$verdict" == "SOFT" ]]; then
  waited=0
  while (( waited < MAXWAIT )); do
    sleep "$POLL"
    waited=$(( waited + POLL ))
    IFS='|' read -r verdict name value status reset <<<"$(check 0)"
    if [[ "$verdict" == "SPENT" ]]; then
      stop "$name" "$status"
    fi
    if [[ "$verdict" == "HARD" ]]; then
      deny "clusage guard rail: the 7d limit is at ${value}% (hard cut at ${HARD}%). $(retry 7d "$reset")"
    fi
    if [[ "$verdict" == "OK" ]]; then
      echo "clusage guard rail: 5h usage back down to ${value}%, work resumed after ${waited}s." >&2
      date +%s > "$STATE"
      exit 0
    fi
  done
  deny "clusage guard rail: the 5h limit is at ${value}% and did not drop in ${MAXWAIT}s (soft limit ${SOFT}%). $(retry 5h "$reset")"
fi

date +%s > "$STATE"
exit 0
