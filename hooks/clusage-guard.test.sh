#!/usr/bin/env bash
# Checks the guard rail decisions against fixed usage tables.
set -uo pipefail
GUARD="$(dirname "$0")/clusage-guard.sh"
GUARD_ABS="$(cd "$(dirname "$0")" && pwd)/clusage-guard.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
pass=0 fail=0

run() { # run <fixture-text> <expect: allow|deny> <expect-substring> [stdin-json]
  printf '%s\n' "$1" > "$TMP/fx"
  rm -f "${TMPDIR:-/tmp}/clusage-guard-${USER:-x}.stamp"
  if [[ -n "${4:-}" ]]; then
    out=$(printf '%s' "$4" | CLUSAGE_GUARD_FIXTURE="$TMP/fx" CLUSAGE_GUARD_POLL=1 \
          CLUSAGE_GUARD_MAXWAIT=2 bash "$GUARD" 2>/dev/null)
  else
    out=$(CLUSAGE_GUARD_FIXTURE="$TMP/fx" CLUSAGE_GUARD_POLL=1 CLUSAGE_GUARD_MAXWAIT=2 \
          bash "$GUARD" </dev/null 2>/dev/null)
  fi
  if [[ "$2" == allow ]]; then
    [[ -z "$out" ]] && { pass=$((pass+1)); return; }
  else
    [[ "$out" == *'"deny"'* && "$out" == *"$3"* ]] && { pass=$((pass+1)); return; }
  fi
  fail=$((fail+1)); echo "FAIL: expected $2 $3, got: ${out:-<empty>}"
}

low="5h  33% used  allowed  resets Wed 19:30 (in 4h36m)
7d  20% used  allowed  resets Mon 18:00 (in 123h6m)
overage 78% used allowed_warning"
run "$low" allow

high5="5h  94% used  allowed_warning  resets Wed 19:30 (in 4h36m)
7d  20% used  allowed"
run "$high5" deny "5h limit is at 94% and did not drop"

# just under the soft default, so the call goes through
near="5h  89% used  allowed_warning
7d  20% used  allowed"
run "$near" allow

high7="5h  33% used  allowed
7d  96% used  allowed_warning"
run "$high7" deny "7d limit is at 96%"

opus="5h  10% used  allowed
7d  40% used  allowed
7d-opus  97% used  allowed_warning"
run "$opus" deny "7d limit is at 97%"

edge="5h  90% used  allowed
7d  95% used  allowed"
run "$edge" deny "7d limit is at 95%"

soft_edge="5h  90% used  allowed
7d  20% used  allowed"
run "$soft_edge" deny "5h limit is at 90%"

# a deny names the reset clock time and tells the caller to wait for it
run "$high5" deny "It resets Wed 19:30 (in 4h36m). Set a timer"
# a window with no reset leaves nothing to wait for
run "$high7" deny "reported no reset time"

# an exhausted window means overage pays for the call, so stop at once
burned="5h  100% used  rejected  resets Wed 19:30 (in 4h36m)
7d  60% used  allowed
overage 12% used allowed"
run "$burned" deny "5h window is exhausted (status rejected)"

# opting in drops back to the ordinary soft threshold path
printf '%s\n' "$burned" > "$TMP/fx"
rm -f "${TMPDIR:-/tmp}/clusage-guard-${USER:-x}.stamp"
out=$(CLUSAGE_GUARD_ALLOW_OVERAGE=1 CLUSAGE_GUARD_FIXTURE="$TMP/fx" CLUSAGE_GUARD_POLL=1 \
      CLUSAGE_GUARD_MAXWAIT=2 bash "$GUARD" </dev/null 2>/dev/null)
[[ "$out" == *"5h limit is at 100% and did not drop"* ]] \
  && pass=$((pass+1)) || { fail=$((fail+1)); echo "FAIL: overage opt-in, got: ${out:-<empty>}"; }

# a scheduling tool passes a tripped window, so the agent can book the retry
run "$high7" allow "" '{"hook_event_name":"PreToolUse","tool_name":"ScheduleWakeup","tool_input":{}}'
run "$high7" deny "7d limit is at 96%" '{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{}}'

# the off switch short circuits before any check
off="$TMP/off"
mkdir -p "$off"
: > "$off/clusage-guard.off"
printf '%s\n' "$high7" > "$TMP/fx"
rm -f "${TMPDIR:-/tmp}/clusage-guard-${USER:-x}.stamp"
out=$(CLAUDE_CONFIG_DIR="$off" CLUSAGE_GUARD_FIXTURE="$TMP/fx" bash "$GUARD" </dev/null 2>/dev/null)
[[ -z "$out" ]] && pass=$((pass+1)) || { fail=$((fail+1)); echo "FAIL: off switch, got: $out"; }
out=$(CLAUDE_CONFIG_DIR="$off" bash "$GUARD" --status 2>/dev/null)
[[ "$out" == *"off switch present"* ]] \
  && pass=$((pass+1)) || { fail=$((fail+1)); echo "FAIL: --status did not report the off switch"; }
rm -rf "$off"

# resume path: paused at 84%, drops to 33% while polling
printf '%s\n' "$high5" > "$TMP/fx"
rm -f "${TMPDIR:-/tmp}/clusage-guard-${USER:-x}.stamp"
( sleep 2; printf '%s\n' "$low" > "$TMP/fx" ) &
out=$(CLUSAGE_GUARD_FIXTURE="$TMP/fx" CLUSAGE_GUARD_POLL=1 CLUSAGE_GUARD_MAXWAIT=10 \
      bash "$GUARD" </dev/null 2>"$TMP/err")
wait
if [[ -z "$out" && "$(cat "$TMP/err")" == *"work resumed"* ]]; then
  pass=$((pass+1))
else
  fail=$((fail+1)); echo "FAIL: resume, out=${out:-<empty>} err=$(cat "$TMP/err")"
fi

rm -f "${TMPDIR:-/tmp}/clusage-guard-${USER:-x}.stamp"

# registration round trip against a throwaway CLAUDE_CONFIG_DIR
cfg="$TMP/claude"
mkdir -p "$cfg"
printf '{"model":"opus","hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"x"}]}]}}\n' > "$cfg/settings.json"
export CLAUDE_CONFIG_DIR="$cfg"
bash "$GUARD" --status >/dev/null 2>&1 && { fail=$((fail+1)); echo "FAIL: status before install"; } || pass=$((pass+1))
bash "$GUARD" --install >/dev/null || { fail=$((fail+1)); echo "FAIL: install"; }
bash "$GUARD" --install >/dev/null   # idempotent
bash "$GUARD" --status >/dev/null && pass=$((pass+1)) || { fail=$((fail+1)); echo "FAIL: status after install"; }
[[ -L "$cfg/hooks/clusage-guard.sh" && "$(readlink "$cfg/hooks/clusage-guard.sh")" == "$GUARD_ABS" ]] \
  && pass=$((pass+1)) || { fail=$((fail+1)); echo "FAIL: symlink not created"; }
grep -q "$cfg/hooks/clusage-guard.sh" "$cfg/settings.json" \
  && pass=$((pass+1)) || { fail=$((fail+1)); echo "FAIL: settings.json names the source path, not the link"; }
bash "$cfg/hooks/clusage-guard.sh" --status >/dev/null \
  && pass=$((pass+1)) || { fail=$((fail+1)); echo "FAIL: running via the link broke"; }
n=$(python3 -c 'import json,sys;print(len(json.load(open(sys.argv[1]))["hooks"]["PreToolUse"]))' "$cfg/settings.json")
[[ "$n" == 1 ]] && pass=$((pass+1)) || { fail=$((fail+1)); echo "FAIL: install not idempotent, got $n entries"; }
head -c 20 "$cfg/settings.json" | grep -q '"model"' && pass=$((pass+1)) || { fail=$((fail+1)); echo "FAIL: key order not preserved"; }
bash "$GUARD" --uninstall >/dev/null
python3 -c 'import json,sys;d=json.load(open(sys.argv[1]));sys.exit(0 if "PreToolUse" not in d.get("hooks",{}) and "SessionStart" in d["hooks"] else 1)' "$cfg/settings.json" \
  && pass=$((pass+1)) || { fail=$((fail+1)); echo "FAIL: uninstall left residue"; }
[[ ! -e "$cfg/hooks/clusage-guard.sh" ]] \
  && pass=$((pass+1)) || { fail=$((fail+1)); echo "FAIL: uninstall left the symlink"; }

# a real file at the link path is left alone
mkdir -p "$cfg/hooks"; echo "mine" > "$cfg/hooks/clusage-guard.sh"
bash "$GUARD" --install >/dev/null 2>&1 \
  && { fail=$((fail+1)); echo "FAIL: install clobbered a real file"; } \
  || { [[ "$(cat "$cfg/hooks/clusage-guard.sh")" == mine ]] && pass=$((pass+1)) \
       || { fail=$((fail+1)); echo "FAIL: real file was overwritten"; }; }
rm -f "$cfg/hooks/clusage-guard.sh"

# install links to the path it was called by, not to the resolved path
mkdir -p "$TMP/versioned/hooks"
cp "$GUARD_ABS" "$TMP/versioned/hooks/clusage-guard.sh"
ln -s "$TMP/versioned" "$TMP/stable"
bash "$TMP/stable/hooks/clusage-guard.sh" --install >/dev/null
[[ "$(readlink "$cfg/hooks/clusage-guard.sh")" == "$TMP/stable/hooks/clusage-guard.sh" ]] \
  && pass=$((pass+1)) \
  || { fail=$((fail+1)); echo "FAIL: link resolved past the stable path, got $(readlink "$cfg/hooks/clusage-guard.sh")"; }
bash "$TMP/stable/hooks/clusage-guard.sh" --status | grep -q "stable/hooks/clusage-guard.sh$" \
  && pass=$((pass+1)) || { fail=$((fail+1)); echo "FAIL: status resolved past the link target"; }
bash "$GUARD" --uninstall >/dev/null
unset CLAUDE_CONFIG_DIR

echo "pass=$pass fail=$fail"
[[ $fail -eq 0 ]]
