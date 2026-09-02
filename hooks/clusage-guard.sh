#!/usr/bin/env bash
# clusage guard rail: a Claude Code PreToolUse hook.
#
# It pauses tool calls while the 5h rate limit window sits at or above a soft
# threshold, and denies them once a 7d window passes a hard threshold. The
# numbers come from `clusage usage`.
#
# Register it with `clusage hook install`, or run this script directly with
# --install. Install links the script into the Claude Code hooks directory and
# names that link in settings.json, so an upgrade of clusage upgrades the hook.
# Run --status to see the current state, --uninstall to remove it.
#
# Config (environment):
#   CLUSAGE_GUARD_DISABLE=1     turn the guard off
#   CLUSAGE_GUARD_5H=90         soft threshold, percent, pause and poll
#   CLUSAGE_GUARD_7D=95         hard threshold, percent, deny without polling
#   CLUSAGE_GUARD_INTERVAL=360  seconds between checks while under threshold
#   CLUSAGE_GUARD_POLL=15       seconds between checks while paused
#   CLUSAGE_GUARD_MAXWAIT=45    deny after pausing this long
#   CLUSAGE_GUARD_FIXTURE=path  read usage text from a file instead of clusage
set -uo pipefail

SOFT=${CLUSAGE_GUARD_5H:-90}
HARD=${CLUSAGE_GUARD_7D:-95}
INTERVAL=${CLUSAGE_GUARD_INTERVAL:-360}
POLL=${CLUSAGE_GUARD_POLL:-15}
# A hook that blocks for minutes makes the Claude Code session look dead, and
# the app kills it. Wait only for a short spike, then hand the decision back.
MAXWAIT=${CLUSAGE_GUARD_MAXWAIT:-45}
STATE="${TMPDIR:-/tmp}/clusage-guard-${USER:-x}.stamp"
# Absolute, but deliberately unresolved. The caller may name this script
# through a stable path such as <brew prefix>/share/clusage/hooks, and
# resolving it would bury a version number in the link that --install makes.
SELF=${BASH_SOURCE[0]}
[[ "$SELF" != /* ]] && SELF="$PWD/$SELF"
CLAUDE_DIR=${CLAUDE_CONFIG_DIR:-$HOME/.claude}
LINK="$CLAUDE_DIR/hooks/clusage-guard.sh"

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

hooks = settings.setdefault("hooks", collections.OrderedDict())
pre = hooks.setdefault("PreToolUse", [])
mine = [e for e in pre
        if any("clusage-guard" in h.get("command", "") for h in e.get("hooks", []))]

if mode == "status":
    if not mine:
        print("not registered in", path)
        sys.exit(1)
    for entry in mine:
        for hook in entry["hooks"]:
            print("registered:", hook["command"], "(timeout %ss)" % hook.get("timeout", 60))
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
    for entry in mine:
        for hook in entry["hooks"]:
            if "clusage-guard" in hook.get("command", ""):
                hook["command"], hook["timeout"] = command, timeout
    if not mine:
        pre.append(collections.OrderedDict([
            ("matcher", "*"),
            ("hooks", [collections.OrderedDict([
                ("type", "command"), ("command", command), ("timeout", timeout)])]),
        ]))
    action = "installed"
else:
    hooks["PreToolUse"] = [e for e in pre if e not in mine]
    if not hooks["PreToolUse"]:
        del hooks["PreToolUse"]
    if not hooks:
        del settings["hooks"]
    action = "removed" if mine else "was not registered"

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
  --status)    manage status; exit $? ;;
  "") ;;
  *) echo "clusage-guard: unknown flag $1 (want: --install, --uninstall, --status)" >&2; exit 1 ;;
esac

# --- the guard rail ---------------------------------------------------------

[[ "${CLUSAGE_GUARD_DISABLE:-0}" == 1 ]] && exit 0

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

# pct <table> <window prefix>. Highest percent of the matching windows, or -1.
pct() {
  awk -v p="$2" '$1 ~ "^"p && $2 ~ /%$/ {
    gsub("%","",$2); if ($2+0 > m) m = $2+0
  } END { print (m == "" ? -1 : m) }' <<<"$1"
}

check() {
  local table five seven
  table=$(read_usage "$1")
  [[ -z "$table" ]] && return 0   # clusage unavailable, fail open
  five=$(pct "$table" "5h")
  seven=$(pct "$table" "7d")
  if (( seven >= HARD )); then echo "HARD ${seven}"; return 0; fi
  if (( five >= SOFT )); then echo "SOFT ${five}"; return 0; fi
  echo "OK ${five}"
}

now=$(date +%s)
last=0
[[ -f "$STATE" ]] && last=$(cat "$STATE" 2>/dev/null || echo 0)
(( now - last < INTERVAL )) && exit 0

read -r verdict value <<<"$(check 5)"

if [[ "$verdict" == "HARD" ]]; then
  deny "clusage guard rail: the 7d limit is at ${value}% (hard cut at ${HARD}%). Stop all work now, in this agent and in every subagent. Do not retry. Report the limit to the user and end the turn."
fi

if [[ "$verdict" == "SOFT" ]]; then
  waited=0
  while (( waited < MAXWAIT )); do
    sleep "$POLL"
    waited=$(( waited + POLL ))
    read -r verdict value <<<"$(check 0)"
    if [[ "$verdict" == "HARD" ]]; then
      deny "clusage guard rail: the 7d limit is at ${value}% (hard cut at ${HARD}%). Stop all work now, in this agent and in every subagent. Do not retry."
    fi
    if [[ "$verdict" == "OK" ]]; then
      echo "clusage guard rail: 5h usage back down to ${value}%, work resumed after ${waited}s." >&2
      date +%s > "$STATE"
      exit 0
    fi
  done
  deny "clusage guard rail: the 5h limit is at ${value}% and did not drop in ${MAXWAIT}s (soft limit ${SOFT}%). Stop all work now, in this agent and in every subagent. Do not retry. Tell the user the session is throttled and end the turn. The next message they send is checked again, and work continues once the window drops."
fi

date +%s > "$STATE"
exit 0
