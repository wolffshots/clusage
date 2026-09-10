#!/usr/bin/env bash
# Checks the guard rail decisions against fixed usage tables.
set -uo pipefail
GUARD="$(dirname "$0")/clusage-guard.sh"
GUARD_ABS="$(cd "$(dirname "$0")" && pwd)/clusage-guard.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
pass=0 fail=0

# The guard writes its stamp to one shared path per user. Every Claude Code
# session on this machine runs the guard, so a live session would race these
# tests. Give this run its own stamp inside the throwaway directory.
export CLUSAGE_GUARD_STATE="$TMP/stamp"
STAMP="$CLUSAGE_GUARD_STATE"
# The guard reads its thresholds through `clusage guard-config`. Point clusage
# at a throwaway config directory, so these tests measure the defaults rather
# than whatever the machine running them has configured.
export XDG_CONFIG_HOME="$TMP/config"

run() { # run <fixture-text> <expect: allow|deny> <expect-substring> [stdin-json]
  printf '%s\n' "$1" > "$TMP/fx"
  rm -f "$STAMP"
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
  fail=$((fail+1)); echo "FAIL: expected $2 ${3:-}, got: ${out:-<empty>}"
}

low="5h  33% used  allowed  resets Wed 19:30 (in 4h36m)
7d  20% used  allowed  resets Mon 18:00 (in 123h6m)
overage 78% used allowed_warning"
run "$low" allow

low_rate="5h  33% used  allowed  14.2%/h  resets Wed 19:30 (in 4h36m)
7d  20% used  allowed  0.9%/h  resets Mon 18:00 (in 123h6m)
overage 78% used allowed_warning"

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

# a percent can carry a fraction, which bash arithmetic cannot read
frac7="5h  33% used  allowed
7d  95.5% used  allowed_warning"
run "$frac7" deny "7d limit is at 95.5%"

frac5="5h  90.5% used  allowed_warning
7d  20% used  allowed"
run "$frac5" deny "5h limit is at 90.5%"

# just under the hard cut, so a fraction must not round up into a deny
under7="5h  33% used  allowed
7d  94.5% used  allowed_warning"
run "$under7" allow

soft_edge="5h  90% used  allowed
7d  20% used  allowed"
run "$soft_edge" deny "5h limit is at 90%"

# a deny names the reset clock time and tells the caller to stop and ask
run "$high5" deny "It resets Wed 19:30 (in 4h36m). Stop all other work now"
# a window with no reset leaves nothing to wait for
run "$high7" deny "reported no reset time"

# an exhausted window means overage pays for the call, so stop at once
burned="5h  100% used  rejected  resets Wed 19:30 (in 4h36m)
7d  60% used  allowed
overage 12% used allowed"
run "$burned" deny "5h window is exhausted (status rejected)"

# A row with no status header puts the rate in the status field. A rate is not
# a status, so the window is not exhausted and the call goes through.
no_status="5h  61% used              14.2%/h  resets Wed 19:30 (in 4h)
7d  20% used  allowed"
run "$no_status" allow ""

# --- no usable window denies ------------------------------------------------

# The guard judges a percent. With no percent to judge it cannot tell headroom
# from an exhausted window, so it denies rather than assume there is room.
run "" deny "no usable rate limit window"
run "clusage: no usable anthropic-ratelimit-unified-* headers on the response" \
  deny "no usable rate limit window"
# A partial table is the same condition. A missing 7d row leaves the hard cut
# unenforced, which is exactly what this deny exists to prevent.
run "5h  61% used  allowed  resets Wed 19:30 (in 4h)" deny "no usable rate limit window"
run "7d  20% used  allowed" deny "no usable rate limit window"
# The deny has to carry its own way out, because a denied agent cannot repair
# clusage and cannot set an environment variable for the session.
run "" deny "touch "
run "" deny "clusage usage -force"

# opting in drops back to the ordinary soft threshold path
printf '%s\n' "$burned" > "$TMP/fx"
rm -f "$STAMP"
out=$(CLUSAGE_GUARD_ALLOW_OVERAGE=1 CLUSAGE_GUARD_FIXTURE="$TMP/fx" CLUSAGE_GUARD_POLL=1 \
      CLUSAGE_GUARD_MAXWAIT=2 bash "$GUARD" </dev/null 2>/dev/null)
[[ "$out" == *"5h limit is at 100% and did not drop"* ]] \
  && pass=$((pass+1)) || { fail=$((fail+1)); echo "FAIL: overage opt-in, got: ${out:-<empty>}"; }

# a scheduling tool passes a tripped window, so the agent can book the retry
run "$high7" allow "" '{"hook_event_name":"PreToolUse","tool_name":"ScheduleWakeup","tool_input":{}}'
run "$high7" deny "7d limit is at 96%" '{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{}}'
# a deny asks the agent to question the user, so the question tool must pass too
run "$high7" allow "" '{"hook_event_name":"PreToolUse","tool_name":"AskUserQuestion","tool_input":{}}'

# the off switch short circuits before any check
off="$TMP/off"
mkdir -p "$off"
: > "$off/clusage-guard.off"
printf '%s\n' "$high7" > "$TMP/fx"
rm -f "$STAMP"
out=$(CLAUDE_CONFIG_DIR="$off" CLUSAGE_GUARD_FIXTURE="$TMP/fx" bash "$GUARD" </dev/null 2>/dev/null)
[[ -z "$out" ]] && pass=$((pass+1)) || { fail=$((fail+1)); echo "FAIL: off switch, got: $out"; }
out=$(CLAUDE_CONFIG_DIR="$off" bash "$GUARD" --status 2>/dev/null)
[[ "$out" == *"off switch present"* ]] \
  && pass=$((pass+1)) || { fail=$((fail+1)); echo "FAIL: --status did not report the off switch"; }
rm -rf "$off"

# resume path: paused at 84%, drops to 33% while polling
printf '%s\n' "$high5" > "$TMP/fx"
rm -f "$STAMP"
( sleep 2; printf '%s\n' "$low" > "$TMP/fx" ) &
out=$(CLUSAGE_GUARD_FIXTURE="$TMP/fx" CLUSAGE_GUARD_POLL=1 CLUSAGE_GUARD_MAXWAIT=10 \
      bash "$GUARD" </dev/null 2>"$TMP/err")
wait
if [[ -z "$out" && "$(cat "$TMP/err")" == *"work resumed"* ]]; then
  pass=$((pass+1))
else
  fail=$((fail+1)); echo "FAIL: resume, out=${out:-<empty>} err=$(cat "$TMP/err")"
fi

rm -f "$STAMP"

# --- the poll interval ------------------------------------------------------

iv() { # iv <5h percent> <7d percent> <expect seconds>
  got=$(bash "$GUARD" --interval "$1" "$2" 2>/dev/null)
  [[ "$got" == "$3" ]] && { pass=$((pass+1)); return; }
  fail=$((fail+1)); echo "FAIL: interval $1/$2 expected $3, got ${got:-<empty>}"
}

# the ramp is quadratic in the closer window, over the 90/95 defaults
iv 0 0 300
iv 10 5 297
iv 50 20 217
iv 70 30 137
iv 89 20 36
iv 90 20 30
iv 100 20 30
# the 7d window drives the wait when it is the closer of the two to its cut
iv 20 90 58
iv 20 95 30
# a missing or unreadable percent reads as no load
iv -1 -1 300
iv abc abc 300
iv "" "" 300

# a floor above the ceiling is a typo, so the ceiling wins
got=$(CLUSAGE_GUARD_INTERVAL=60 CLUSAGE_GUARD_INTERVAL_MIN=900 \
      bash "$GUARD" --interval 10 5)
[[ "$got" == 60 ]] && pass=$((pass+1)) \
  || { fail=$((fail+1)); echo "FAIL: floor above ceiling, got $got"; }
# a zero ceiling still means check on every call
got=$(CLUSAGE_GUARD_INTERVAL=0 bash "$GUARD" --interval 10 5)
[[ "$got" == 0 ]] && pass=$((pass+1)) \
  || { fail=$((fail+1)); echo "FAIL: zero ceiling, got $got"; }
# equal bounds give back the old fixed interval
got=$(CLUSAGE_GUARD_INTERVAL=360 CLUSAGE_GUARD_INTERVAL_MIN=360 \
      bash "$GUARD" --interval 88 20)
[[ "$got" == 360 ]] && pass=$((pass+1)) \
  || { fail=$((fail+1)); echo "FAIL: equal bounds, got $got"; }

# --- a bad bound falls back instead of breaking the guard --------------------

# A ceiling that is not a number must not read as zero, which would turn the
# ramp into a probe on every call.
for bad in abc "" -5 3.5; do
  got=$(CLUSAGE_GUARD_INTERVAL="$bad" bash "$GUARD" --interval 10 5)
  [[ "$got" == 297 ]] && pass=$((pass+1)) \
    || { fail=$((fail+1)); echo "FAIL: ceiling '$bad' expected 297, got ${got:-<empty>}"; }
done
# The floor keeps the same treatment.
got=$(CLUSAGE_GUARD_INTERVAL_MIN=abc bash "$GUARD" --interval 90 20)
[[ "$got" == 30 ]] && pass=$((pass+1)) \
  || { fail=$((fail+1)); echo "FAIL: floor abc expected 30, got ${got:-<empty>}"; }

# A bad pause bound killed the hook the moment it reached the pause: bash read
# MAXWAIT as a variable name in arithmetic, and sleep rejected POLL outright.
# Either way the hook exited before it could deny, and the tool call went
# through. Both now fall back, so the hook must still be pausing a few seconds
# in. Waiting out the whole fallback pause would cost the suite a minute, so
# check that it survived the pause instead.
for bad in CLUSAGE_GUARD_MAXWAIT=abc CLUSAGE_GUARD_POLL=abc; do
  printf '%s\n' "$high5" > "$TMP/fx"
  rm -f "$STAMP"
  timeout 3 env CLUSAGE_GUARD_FIXTURE="$TMP/fx" "$bad" \
    bash "$GUARD" </dev/null >/dev/null 2>"$TMP/err"
  rc=$?
  if [[ $rc == 124 && "$(cat "$TMP/err")" != *"unbound variable"* \
        && "$(cat "$TMP/err")" != *"invalid time interval"* ]]; then
    pass=$((pass+1))
  else
    fail=$((fail+1))
    echo "FAIL: $bad died in the pause (rc=$rc): $(cat "$TMP/err")"
  fi
done

# POLL=0 never advances the wait, so the pause loop never ends. A hung hook is
# killed on timeout, which lets the call through, so zero must not survive.
printf '%s\n' "$high5" > "$TMP/fx"
rm -f "$STAMP"
out=$(CLUSAGE_GUARD_FIXTURE="$TMP/fx" CLUSAGE_GUARD_MAXWAIT=2 CLUSAGE_GUARD_POLL=0 \
      timeout 20 bash "$GUARD" </dev/null 2>/dev/null)
rc=$?
if [[ $rc != 124 && "$out" == *'"deny"'* ]]; then
  pass=$((pass+1))
else
  fail=$((fail+1)); echo "FAIL: POLL=0 hung or allowed (rc=$rc), got: ${out:-<empty>}"
fi

# --- the projection ---------------------------------------------------------

pj() { # pj <percent> <rate> <cut> <expect seconds, or empty>
  got=$(bash "$GUARD" --project "$1" "$2" "$3" 2>/dev/null)
  [[ "$got" == "$4" ]] && { pass=$((pass+1)); return; }
  fail=$((fail+1)); echo "FAIL: project $1/$2/$3 expected '${4:-<empty>}', got '${got:-<empty>}'"
}

# 60 points of headroom at 30 points per hour is 2h, and a quarter of that is
# 1800s.
pj 30 30 90 1800
# 1 point of headroom at 30 points per hour is 2 minutes, a quarter is 30s.
pj 89 30 90 30
# an unknown, zero or negative rate has nothing to project
pj 30 "" 90 ""
pj 30 0 90 ""
pj 30 -5 90 ""
# a cut already behind us has nothing to project
pj 95 30 90 ""

# --- the stamp gate ---------------------------------------------------------

gate() { # gate <stamp contents> <expect: probe|skip> <label>
  # high7 denies every call, so a probe shows as output and a skip as silence.
  printf '%s\n' "$high7" > "$TMP/fx"
  if [[ "$1" == "-" ]]; then rm -f "$STAMP"; else printf '%s\n' "$1" > "$STAMP"; fi
  out=$(CLUSAGE_GUARD_FIXTURE="$TMP/fx" bash "$GUARD" </dev/null 2>/dev/null)
  if [[ "$2" == probe ]]; then
    [[ "$out" == *'"deny"'* ]] && { pass=$((pass+1)); return; }
  else
    [[ -z "$out" ]] && { pass=$((pass+1)); return; }
  fi
  fail=$((fail+1)); echo "FAIL: gate $3 expected $2, got: ${out:-<empty>}"
}

age() { echo $(( $(date +%s) - $1 )); }

# 10 percent waits about 297s, so a check 100s old still holds
gate "$(age 100) 10 5" skip "low usage, recent check"
# 90 percent waits 30s, so the same age must probe again
gate "$(age 100) 90 20" probe "high usage, recent check"
# the 7d window drives the wait on its own
gate "$(age 100) 20 92" probe "high 7d, recent check"
# a stamp older than the longest wait always probes
gate "$(age 400) 10 5" probe "low usage, stale check"
# a stamp from the older build holds the time alone, which reads as no load
gate "$(age 100)" skip "old stamp format"
gate "$(age 400)" probe "old stamp format, stale"
# nothing usable in the stamp means check now
gate "garbage" probe "unreadable stamp"
gate "" probe "empty stamp"
gate "-" probe "no stamp"

# --- the projection tightens the gate ---------------------------------------

# 10 percent with no rate waits about 297s, so a 100s old check holds.
gate "$(age 100) 10 5 -1 -1" skip "low usage, no rate"
# The same level climbing at 60 points per hour reaches the 90 percent cut in
# 80 minutes. A quarter of that is 1200s, which is still slower than the ramp,
# so the ramp still wins and the check holds.
gate "$(age 100) 10 5 60 0" skip "low usage, slow climb"
# At 3000 points per hour the 90 percent cut is 96 seconds away. A quarter of
# that is 24s, which the floor raises to 30s, so a 100s old check is stale and
# the guard must probe even though the level is only 10 percent. This is the
# whole point of the projection term.
gate "$(age 100) 10 5 3000 0" probe "low usage, violent climb"
# The 7d rate drives the gate on its own. At 3000 points per hour the 95
# percent cut is 108 seconds away, and a quarter of that is 27s, which the
# floor raises to 30s. Deleting the 7d projection term leaves the ramp at
# about 297s, so a 100s old check would hold and this case would fail.
gate "$(age 100) 5 10 0 3000" probe "high 7d rate drives the gate"
# A window already near its cut probes whatever the rate says.
gate "$(age 100) 88 5 -1 -1" probe "near the cut, no rate"
# A v0.8.0 stamp carries no rates and must behave exactly as before.
gate "$(age 100) 10 5" skip "v0.8.0 stamp, low usage"
gate "$(age 400) 10 5" probe "v0.8.0 stamp, stale"

# Zero means check on every call, so the floor must not raise it. A brand new
# stamp still probes. A floor applied without that guard would skip instead.
printf '%s\n' "$high7" > "$TMP/fx"
printf '%s\n' "$(age 0) 10 5 -1 -1" > "$STAMP"
out=$(CLUSAGE_GUARD_INTERVAL=0 CLUSAGE_GUARD_FIXTURE="$TMP/fx" bash "$GUARD" \
      </dev/null 2>/dev/null)
[[ "$out" == *'"deny"'* ]] && pass=$((pass+1)) \
  || { fail=$((fail+1)); echo "FAIL: gate interval 0 expected probe, got: ${out:-<empty>}"; }

# A floor above the ceiling is a typo, and interval_for returns the ceiling.
# The gate must not raise that result back to the floor. Here the ramp asks for
# 60s, so a 100s old check is stale and the guard must probe. The stamp holds
# three fields, so no rate is involved. A floor applied to the result would
# wait 900s and stay silent, which allows a call that v0.8.0 denies.
printf '%s\n' "$high7" > "$TMP/fx"
printf '%s\n' "$(age 100) 33 96" > "$STAMP"
out=$(CLUSAGE_GUARD_INTERVAL=60 CLUSAGE_GUARD_INTERVAL_MIN=900 \
      CLUSAGE_GUARD_FIXTURE="$TMP/fx" bash "$GUARD" </dev/null 2>/dev/null)
[[ "$out" == *'"deny"'* ]] && pass=$((pass+1)) \
  || { fail=$((fail+1)); echo "FAIL: gate floor above ceiling expected probe, got: ${out:-<empty>}"; }

# A floor that is not a number reaches the projection loop, where bash reads it
# as a variable name and set -u kills the hook before it can deny. The stamp
# carries a rate, so the loop runs. The guard must fall back to the default
# floor and still deny, exactly as v0.8.0 does.
printf '%s\n' "$high7" > "$TMP/fx"
printf '%s\n' "$(age 400) 10 5 60 0" > "$STAMP"
out=$(CLUSAGE_GUARD_INTERVAL_MIN=abc CLUSAGE_GUARD_FIXTURE="$TMP/fx" \
      bash "$GUARD" </dev/null 2>/dev/null)
[[ "$out" == *'"deny"'* ]] && pass=$((pass+1)) \
  || { fail=$((fail+1)); echo "FAIL: gate non-numeric floor expected deny, got: ${out:-<empty>}"; }

# A floor of 030 must read as 30, not the octal 24 a leading zero would give in
# bash arithmetic. A climb of 10 percent at 3000 points per hour toward the 90
# percent cut projects a 24s floor-eligible wait. At floor 30 a 27s old check
# still holds; at floor 24 it would already be stale. Only 30 must survive.
printf '%s\n' "$high7" > "$TMP/fx"
printf '%s\n' "$(age 27) 10 5 3000 0" > "$STAMP"
out=$(CLUSAGE_GUARD_INTERVAL_MIN=030 CLUSAGE_GUARD_FIXTURE="$TMP/fx" \
      bash "$GUARD" </dev/null 2>/dev/null)
[[ -z "$out" ]] && pass=$((pass+1)) \
  || { fail=$((fail+1)); echo "FAIL: INTERVAL_MIN=030 must behave as 30, got: ${out:-<empty>}"; }

# an allowed call records both percents, so the next call can size its wait
rm -f "$STAMP"
printf '%s\n' "$low" > "$TMP/fx"
CLUSAGE_GUARD_FIXTURE="$TMP/fx" bash "$GUARD" </dev/null >/dev/null 2>&1
# The stamp now holds two rates after the percents, which the last variable
# absorbs. This case still proves a table with no rate column stamps both
# percents.
read -r _ sfive sseven _rest < "$STAMP"
[[ "$sfive" == 33 && "$sseven" == 20 ]] && pass=$((pass+1)) \
  || { fail=$((fail+1)); echo "FAIL: stamp percents, got 5h=$sfive 7d=$sseven"; }

# an allowed call records both percents and both rates
rm -f "$STAMP"
printf '%s\n' "$low_rate" > "$TMP/fx"
CLUSAGE_GUARD_FIXTURE="$TMP/fx" bash "$GUARD" </dev/null >/dev/null 2>&1
read -r _ sfive sseven r5 r7 < "$STAMP"
[[ "$sfive" == 33 && "$sseven" == 20 && "$r5" == 14.2 && "$r7" == 0.9 ]] \
  && pass=$((pass+1)) \
  || { fail=$((fail+1)); echo "FAIL: stamp rates, got $sfive $sseven $r5 $r7"; }

# the reading cache tracks the wait, so a fast poll never reads a stale probe
mkdir -p "$TMP/bin"
cat > "$TMP/bin/clusage" <<'EOF'
#!/usr/bin/env bash
# The guard reads its thresholds through "guard-config" before every decision.
# That call is not a probe, so it stays out of the argument log, and it answers
# nothing so the built-in defaults stand.
[[ "$1" == guard-config ]] && exit 0
printf '%s\n' "$*" >> "$ARGLOG"
cat "$FX"
EOF
chmod +x "$TMP/bin/clusage"
export ARGLOG="$TMP/args" FX="$TMP/fx"

: > "$ARGLOG"
printf '%s\n' "$low" > "$TMP/fx"
printf '%s\n' "$(age 400) 10 5" > "$STAMP"
PATH="$TMP/bin:$PATH" bash "$GUARD" </dev/null >/dev/null 2>&1
[[ "$(cat "$ARGLOG")" == "usage -threshold 4" ]] && pass=$((pass+1)) \
  || { fail=$((fail+1)); echo "FAIL: cache minutes at low usage, got $(cat "$ARGLOG")"; }

# near the soft cut the wait is under a minute, so the probe must be live
: > "$ARGLOG"
printf '%s\n' "$(age 400) 89 20" > "$STAMP"
PATH="$TMP/bin:$PATH" bash "$GUARD" </dev/null >/dev/null 2>&1
[[ "$(cat "$ARGLOG")" == "usage -threshold 0" ]] && pass=$((pass+1)) \
  || { fail=$((fail+1)); echo "FAIL: cache minutes near the cut, got $(cat "$ARGLOG")"; }

# every poll during a pause reads a live number
: > "$ARGLOG"
printf '%s\n' "$high5" > "$TMP/fx"
rm -f "$STAMP"
PATH="$TMP/bin:$PATH" CLUSAGE_GUARD_POLL=1 CLUSAGE_GUARD_MAXWAIT=2 \
  bash "$GUARD" </dev/null >/dev/null 2>&1
[[ "$(sed -n '2,$p' "$ARGLOG" | sort -u)" == "usage -threshold 0" ]] \
  && pass=$((pass+1)) \
  || { fail=$((fail+1)); echo "FAIL: pause polls not live, got $(cat "$ARGLOG")"; }
unset ARGLOG FX
rm -f "$STAMP"

# --- the config file --------------------------------------------------------

# A clusage that answers guard-config with a lower soft cut, and serves the
# usage table from the same fixture. No CLUSAGE_GUARD_FIXTURE here, because
# that switch also turns the config read off.
cat > "$TMP/bin/clusage" <<'EOF'
#!/usr/bin/env bash
if [[ "$1" == guard-config ]]; then
  cat "$CFG"
  exit 0
fi
cat "$FX"
EOF
chmod +x "$TMP/bin/clusage"
export CFG="$TMP/cfg" FX="$TMP/fx"
printf 'soft_5h=50\nhard_7d=95\ninterval=0\ninterval_min=0\npoll=1\nmaxwait=2\nallow_overage=0\nallow_tools=Solo\n' > "$CFG"
mid="5h  60% used  allowed  resets Wed 19:30 (in 4h36m)
7d  20% used  allowed"
printf '%s\n' "$mid" > "$TMP/fx"

# the config file lowers the soft cut, so 60% now pauses and then denies
rm -f "$STAMP"
out=$(PATH="$TMP/bin:$PATH" bash "$GUARD" </dev/null 2>/dev/null)
[[ "$out" == *'"deny"'* && "$out" == *"soft limit 50%"* ]] && pass=$((pass+1)) \
  || { fail=$((fail+1)); echo "FAIL: config soft cut not applied, got: ${out:-<empty>}"; }

# the config file supplies the poll and the wait as well, so the pause above
# ended on its own rather than on the built-in 45s
rm -f "$STAMP"
start=$(date +%s)
PATH="$TMP/bin:$PATH" bash "$GUARD" </dev/null >/dev/null 2>&1
(( $(date +%s) - start < 10 )) && pass=$((pass+1)) \
  || { fail=$((fail+1)); echo "FAIL: config maxwait not applied, the pause ran long"; }

# an environment variable still wins over the config file
rm -f "$STAMP"
out=$(PATH="$TMP/bin:$PATH" CLUSAGE_GUARD_5H=99 bash "$GUARD" </dev/null 2>/dev/null)
[[ -z "$out" ]] && pass=$((pass+1)) \
  || { fail=$((fail+1)); echo "FAIL: env must override the config soft cut, got: $out"; }

# the config file supplies the allow list, so a tool on it passes unchecked
rm -f "$STAMP"
out=$(printf '{"tool_name":"Solo"}' | PATH="$TMP/bin:$PATH" bash "$GUARD" 2>/dev/null)
[[ -z "$out" ]] && pass=$((pass+1)) \
  || { fail=$((fail+1)); echo "FAIL: config allow list not applied, got: $out"; }

# a clusage that answers nothing leaves the built-in defaults, so 60% passes
cat > "$TMP/bin/clusage" <<'EOF'
#!/usr/bin/env bash
[[ "$1" == guard-config ]] && exit 1
cat "$FX"
EOF
chmod +x "$TMP/bin/clusage"
rm -f "$STAMP"
out=$(PATH="$TMP/bin:$PATH" bash "$GUARD" </dev/null 2>/dev/null)
[[ -z "$out" ]] && pass=$((pass+1)) \
  || { fail=$((fail+1)); echo "FAIL: a broken guard-config must fall back to the defaults, got: $out"; }

# a hostile guard-config cannot inject shell, because no line is eval'd
cat > "$TMP/bin/clusage" <<'EOF'
#!/usr/bin/env bash
if [[ "$1" == guard-config ]]; then
  printf 'soft_5h=50; touch %s/pwned\nallow_tools=$(touch %s/pwned2)\n' "$TMP" "$TMP"
  exit 0
fi
cat "$FX"
EOF
chmod +x "$TMP/bin/clusage"
rm -f "$STAMP"
PATH="$TMP/bin:$PATH" TMP="$TMP" bash "$GUARD" </dev/null >/dev/null 2>&1
[[ ! -e "$TMP/pwned" && ! -e "$TMP/pwned2" ]] && pass=$((pass+1)) \
  || { fail=$((fail+1)); echo "FAIL: guard-config output reached the shell as code"; }
unset CFG FX
rm -f "$STAMP"

# --- the trend clause --------------------------------------------------------

# a deny names the trend when the table carries a rate
high5_rate="5h  94% used  allowed_warning  12.0%/h  resets Wed 19:30 (in 4h36m)
7d  20% used  allowed  0.5%/h"
run "$high5_rate" deny "rising at 12.0%/h"
run "$high5_rate" deny "fills in about 30m"
# with no rate in the table the clause is absent, exactly as v0.8.0
run "$high5" deny "5h limit is at 94% and did not drop"
out=$(printf '%s\n' "$high5" > "$TMP/fx"; rm -f "$STAMP"; \
      CLUSAGE_GUARD_FIXTURE="$TMP/fx" CLUSAGE_GUARD_POLL=1 CLUSAGE_GUARD_MAXWAIT=2 \
      bash "$GUARD" </dev/null 2>/dev/null)
[[ "$out" != *"rising at"* ]] && pass=$((pass+1)) \
  || { fail=$((fail+1)); echo "FAIL: no rate must give no trend clause"; }

# a deny must send the decision to the user, not park the agent on its own
run "$high5" deny "Do not schedule a resume before the user answers"
run "$high5" deny "multiple choice question"
run "$high5" deny "keep working now and pay overage"
run "$high5" deny "only if the user picks"
# a window with no reset has no timer to offer, so it asks a shorter question
run "$high7" deny "reported no reset time"
run "$high7" deny "ask whether to stop here or keep working and pay overage"
run "$high7" deny "Do not decide it yourself"

# a denied agent cannot discover the allow list by trying, so every deny names it
run "$high5" deny "still allows these tools"
run "$high5" deny "AskUserQuestion"
run "$high7" deny "still allows these tools"
run "$burned" deny "still allows these tools"

# the pay-overage option needs a lever the agent can hand to the user
run "$high5" deny "clusage-guard.off"
run "$high7" deny "clusage-guard.off"
run "$burned" deny "clusage-guard.off"

# a long wait must be chained, because a wake-up caps at one hour and a gap
# over 55 minutes expires the prompt cache
run "$high5" deny "legs of 55 minutes or less"
run "$high5" deny "Put the leg number, the total, and the reset time into the message"
run "$high5" deny "On waking, read the leg number from that message"
run "$high5" deny "schedule the next leg and do nothing else. Then end the turn"
run "$high5" deny "Never call a tool to check the clock"
# the no-reset branch has nothing to wait for, so it offers no legs
out=$(printf '%s\n' "$high7" > "$TMP/fx"; rm -f "$STAMP"; \
      CLUSAGE_GUARD_FIXTURE="$TMP/fx" bash "$GUARD" </dev/null 2>/dev/null)
[[ "$out" != *"read the leg number from that message"* ]] && pass=$((pass+1)) \
  || { fail=$((fail+1)); echo "FAIL: no-reset branch must not offer legs"; }

# --- the resume report ------------------------------------------------------

# The fixture below denies every tool call, so a payload that wrongly falls
# through to the guard rail shows up as output instead of passing silently.
printf '%s\n' "$high7" > "$TMP/fx"

resume() { # resume <payload> <expect-substring, empty for no output>
  rm -f "$STAMP"
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
rm -f "$STAMP"

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
