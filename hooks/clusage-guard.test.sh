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

# --- the resume report ------------------------------------------------------

# The fixture below denies every tool call, so a payload that wrongly falls
# through to the guard rail shows up as output instead of passing silently.
printf '%s\n' "$high7" > "$TMP/fx"

resume() { # resume <payload> <expect-substring, empty for no output>
  rm -f "${TMPDIR:-/tmp}/clusage-guard-${USER:-x}.stamp"
  out=$(printf '%s' "$1" | CLUSAGE_GUARD_FIXTURE="$TMP/fx" CLUSAGE_GUARD_POLL=1 \
        CLUSAGE_GUARD_MAXWAIT=2 bash "$GUARD" 2>/dev/null)
  if [[ -z "$2" ]]; then
    [[ -z "$out" ]] && { pass=$((pass+1)); return; }
  else
    [[ "$out" == *"$2"* ]] && { pass=$((pass+1)); return; }
  fi
  fail=$((fail+1)); echo "FAIL: resume expected ${2:-<empty>}, got: ${out:-<empty>}"
}

stale='{"hook_event_name":"SessionStart","source":"resume",
"seconds_since_last_response":5400,"context_tokens":182340,
"prompt_cache_likely_expired":true,"estimated_cache_write_usd":1.1396}'
resume "$stale" '"systemMessage"'
# the desktop app never shows systemMessage, so the same line goes to Claude
resume "$stale" '"additionalContext"'
resume "$stale" "expired after 90m idle"
resume "$stale" 're-sends 182k tokens, about $1.14'

# cache still warm, so there is nothing to say
resume "${stale/true/false}" ""

# an older Claude Code sends the flag without the numbers
resume '{"hook_event_name":"SessionStart","source":"resume",
"prompt_cache_likely_expired":true}' ""

# a fresh session carries none of the fields
resume '{"hook_event_name":"SessionStart","source":"startup"}' ""

out=$(printf '%s' "$stale" | CLUSAGE_RESUME_DISABLE=1 CLUSAGE_GUARD_FIXTURE="$TMP/fx" \
      bash "$GUARD" 2>/dev/null)
[[ -z "$out" ]] && pass=$((pass+1)) || { fail=$((fail+1)); echo "FAIL: disable, got $out"; }

# a tool call still reaches the guard rail, payload and all
resume '{"hook_event_name":"PreToolUse","tool_name":"Bash"}' "7d limit is at 96%"
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
# the session start entry sits beside the one the fixture already had
mine=$(python3 -c '
import json,sys
d=json.load(open(sys.argv[1]))["hooks"]["SessionStart"]
own=[e for e in d if any("clusage-guard" in h.get("command","") for h in e["hooks"])]
print(len(d), len(own), own[0]["matcher"] if own else "-")' "$cfg/settings.json")
[[ "$mine" == "2 1 resume|fork" ]] && pass=$((pass+1)) \
  || { fail=$((fail+1)); echo "FAIL: SessionStart registration, got $mine"; }
bash "$GUARD" --status | grep -q "^registered: SessionStart" \
  && pass=$((pass+1)) || { fail=$((fail+1)); echo "FAIL: status does not list SessionStart"; }
head -c 20 "$cfg/settings.json" | grep -q '"model"' && pass=$((pass+1)) || { fail=$((fail+1)); echo "FAIL: key order not preserved"; }
bash "$GUARD" --uninstall >/dev/null
python3 -c '
import json,sys
h=json.load(open(sys.argv[1])).get("hooks",{})
own=[e for v in h.values() for e in v
     if any("clusage-guard" in c.get("command","") for c in e["hooks"])]
sys.exit(0 if "PreToolUse" not in h and not own
         and h["SessionStart"][0]["hooks"][0]["command"] == "x" else 1)' "$cfg/settings.json" \
  && pass=$((pass+1)) || { fail=$((fail+1)); echo "FAIL: uninstall left residue"; }
[[ ! -e "$cfg/hooks/clusage-guard.sh" ]] \
  && pass=$((pass+1)) || { fail=$((fail+1)); echo "FAIL: uninstall left the symlink"; }

# an install over the PreToolUse-only version adds the new event and keeps the
# rest of the file
old="$TMP/old"
mkdir -p "$old"
printf '{"model":"opus","hooks":{"PreToolUse":[{"matcher":"*","hooks":[{"type":"command","command":"bash %s/hooks/clusage-guard.sh","timeout":60}]}]}}\n' "$old" > "$old/settings.json"
CLAUDE_CONFIG_DIR="$old" bash "$GUARD" --install >/dev/null
upgraded=$(python3 -c '
import json,sys
h=json.load(open(sys.argv[1]))["hooks"]
print(len(h["PreToolUse"]), len(h["SessionStart"]), h["PreToolUse"][0]["hooks"][0]["timeout"])' "$old/settings.json")
[[ "$upgraded" == "1 1 60" ]] && pass=$((pass+1)) \
  || { fail=$((fail+1)); echo "FAIL: upgrade from the older install, got $upgraded"; }
head -c 20 "$old/settings.json" | grep -q '"model"' && pass=$((pass+1)) \
  || { fail=$((fail+1)); echo "FAIL: upgrade lost key order"; }

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
