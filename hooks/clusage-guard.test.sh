#!/usr/bin/env bash
# Checks the guard rail decisions against fixed usage tables.
set -uo pipefail
GUARD="$(dirname "$0")/clusage-guard.sh"
GUARD_ABS="$(cd "$(dirname "$0")" && pwd)/clusage-guard.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
pass=0 fail=0

run() { # run <fixture-text> <expect: allow|deny> <expect-substring>
  printf '%s\n' "$1" > "$TMP/fx"
  rm -f "${TMPDIR:-/tmp}/clusage-guard-${USER:-x}.stamp"
  out=$(CLUSAGE_GUARD_FIXTURE="$TMP/fx" CLUSAGE_GUARD_POLL=1 CLUSAGE_GUARD_MAXWAIT=2 \
        bash "$GUARD" </dev/null 2>/dev/null)
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

high5="5h  84% used  allowed_warning  resets Wed 19:30 (in 4h36m)
7d  20% used  allowed"
run "$high5" deny "5h limit has been at or above 80%"

high7="5h  33% used  allowed
7d  96% used  allowed_warning"
run "$high7" deny "7d limit is at 96%"

opus="5h  10% used  allowed
7d  40% used  allowed
7d-opus  97% used  allowed_warning"
run "$opus" deny "7d limit is at 97%"

edge="5h  80% used  allowed
7d  95% used  allowed"
run "$edge" deny "7d limit is at 95%"

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
bash "$GUARD" --uninstall >/dev/null
unset CLAUDE_CONFIG_DIR

echo "pass=$pass fail=$fail"
[[ $fail -eq 0 ]]
