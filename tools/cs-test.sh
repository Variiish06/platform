#!/usr/bin/env bash
# =============================================================================
# cs-test.sh — end-to-end test for tools/cs, fully isolated:
#   * a PRIVATE tmux server (tmux -L cs-test-<pid>, no ~/.tmux.conf), so the
#     real sessions (platform2, work, ...) are never touched;
#   * a temp config (CS_CONFIG) and temp transcript dir (CS_CLAUDE_PROJECTS);
#   * a FAKE claude (CS_CLAUDE_BIN): a script named `claude` that records its
#     working dir + args to a log and then just sleeps until SIGTERM.
# Exit code: 0 if every check passed, 1 otherwise. Cleans up after itself.
# Run:  ~/platform/tools/cs-test.sh
# =============================================================================
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
CS=$HERE/cs
T=$(mktemp -d "${TMPDIR:-/tmp}/cs-test.XXXXXX")
SOCK=cs-test-$$

export CS_TMUX_SOCKET=$SOCK CS_TMUX_CONF=/dev/null
export CS_CONFIG=$T/projects.conf CS_CLAUDE_PROJECTS=$T/claude-projects
export CS_CLAUDE_BIN=$T/bin/claude CS_STOP_TIMEOUT=5 CS_MIN_FREE_MB=1
LOG=$T/fake-claude.log

tt() { tmux -f /dev/null -L "$SOCK" "$@"; }   # talk to the private server only

cleanup() {
  tt kill-server 2>/dev/null || true
  pkill -TERM -f -- "$T/" 2>/dev/null || true  # any fake claude still alive
  rm -rf -- "$T"
}
trap cleanup EXIT

# --- tiny test framework ---
PASS=0 FAIL=0
ok()   { PASS=$((PASS+1)); printf '  ok   %s\n' "$1"; }
bad()  { FAIL=$((FAIL+1)); printf '  FAIL %s\n' "$1"; [[ -n ${2:-} ]] && printf '%s\n' "$2" | sed 's/^/       > /'; return 0; }
check() { local d=$1; shift; if "$@"; then ok "$d"; else bad "$d"; fi; }
# run cs, keep stdout+stderr in $OUT and the exit code in $RC
run() { set +e; OUT=$("$CS" "$@" 2>&1); RC=$?; set -e; }
has()  { local d=$1 pat=$2; if grep -qE -- "$pat" <<<"$OUT"; then ok "$d"; else bad "$d" "$OUT"; fi; }
hasnt(){ local d=$1 pat=$2; if ! grep -qE -- "$pat" <<<"$OUT"; then ok "$d"; else bad "$d" "$OUT"; fi; }
rc_is(){ local d=$1 want=$2; if [[ $RC == "$want" ]]; then ok "$d"; else bad "$d (rc=$RC, want $want)" "$OUT"; fi; }
rc_nz(){ local d=$1; if [[ $RC != 0 ]]; then ok "$d"; else bad "$d (rc=0, want non-zero)" "$OUT"; fi; }
pid_of() { "$CS" ls --json 2>/dev/null | jq -r --arg n "$1" '.[] | select(.name==$n) | .pids[0] // empty'; }
alive() { kill -0 "$1" 2>/dev/null; }
wait_prompt() { # until the pane's shell is the foreground command with no child
  local i; for i in $(seq 40); do
    [[ $(tt display-message -p -t "$1" '#{pane_current_command}') == bash ]] && return 0; sleep 0.25; done; return 1; }

# --- fixtures ---
mkdir -p "$T/bin" "$T/p/alpha" "$T/p/beta" "$T/p/with space" "$T/p/cur" "$T/anc"
# (#!/bin/bash, not /usr/bin/env: env would re-exec and the process name
#  would become "bash" instead of "claude")
cat > "$T/bin/claude" <<EOF
#!/bin/bash
# fake claude: log "cwd|args", then sleep until SIGTERM (child sleep counts toward RAM)
printf '%s|%s\n' "\$PWD" "\$*" >> "$LOG"
sleep 100000 &
trap 'kill \$! 2>/dev/null; exit 0' TERM
wait
EOF
chmod +x "$T/bin/claude"
cat > "$CS_CONFIG" <<EOF
# test config
alpha  | $T/p/alpha |
beta   | $T/p/beta  | --model fake-model
spaced | $T/p/with space |
ghost  | $T/p/does-not-exist |
cur    | $T/p/cur |
Bad Name | /tmp |
EOF
# alpha and "with space" have saved conversations; beta and cur don't.
for d in "$T/p/alpha" "$T/p/with space"; do
  enc=$(cd -- "$d" && pwd -P); enc=${enc//[^a-zA-Z0-9]/-}
  mkdir -p "$CS_CLAUDE_PROJECTS/$enc"; echo '{}' > "$CS_CLAUDE_PROJECTS/$enc/x.jsonl"
done
touch "$LOG"
# Start the private server with a placeholder session (no ~/.tmux.conf, so no
# plugins/continuum restoring real sessions into it).
tt new-session -d -s zz-idle -c "$T"

echo "== ls with nothing running"
run ls
rc_is "ls exits 0" 0
has   "projects listed as no-tmux" '^ +alpha +no-tmux'
has   "footer says 0 running" '^0 claude session\(s\) running, 0 MiB'
has   "bad config line warned about" "bad name 'Bad Name'"
has   "dir missing is flagged" 'ghost.*\(dir missing\)'

echo "== start: argument errors"
run start 'Bad!'
rc_nz "bad name rejected"; has "bad name message" 'bad name'
run start ghost
rc_nz "missing dir rejected"; has "missing dir message" 'does not exist'
run start nosuch
rc_nz "unknown project rejected"; has "unknown project message" 'not in'

echo "== start: low-memory guard"
CS_MIN_FREE_MB=999999 run start alpha
rc_nz "refuses below CS_MIN_FREE_MB"; has "warns about memory" 'warning: only [0-9]+ MiB'
check "no session created" bash -c "! tmux -f /dev/null -L $SOCK has-session -t =alpha 2>/dev/null"
CS_MIN_FREE_MB=999999 run start alpha --force
rc_is "--force overrides the memory guard" 0

echo "== start: new session"
has   "created tmux session" 'alpha: created tmux session'
has   "reports started pid"  'alpha: started claude \(continue conversation\) pid [0-9]+'
check "fake claude got --continue in alpha dir" grep -qxF "$T/p/alpha|--continue" "$LOG"
ALPHA=$(pid_of alpha)
check "alpha claude pid alive" alive "${ALPHA:-0}"

echo "== start: reuse existing idle session (no transcripts -> plain claude)"
tt new-session -d -s beta -c /
wait_prompt beta || bad "beta shell prompt"
run start beta
rc_is "start beta exits 0" 0
has   "reused session" 'beta: reusing tmux session'
check "plain claude + extra args, cd'd into dir" grep -qxF "$T/p/beta|--model fake-model" "$LOG"

echo "== start: already running is refused"
run start alpha
rc_nz "second start refused"; has "already running message" 'already running there \(pid'

echo "== start --fresh in a directory with spaces"
run start spaced --fresh
rc_is "start spaced exits 0" 0
check "no --continue despite transcripts; spaces intact" grep -qxF "$T/p/with space|" "$LOG"

echo "== ls shows running + RAM + idle"
run ls
has "alpha running with RAM" '^ +alpha +running +[0-9]+[smhd] +[1-9][0-9]*M '
has "spaced dir shown" 'spaced +running .*with space'
has "footer counts 3" '^3 claude session\(s\) running, [1-9][0-9]* MiB'
hasnt "real sessions invisible" 'platform2'
run ram
has "ram shows tree" 'alpha +[0-9]+M +\[[0-9]+\] .*sleep'

echo "== ls --json"
J=$("$CS" ls --json 2>/dev/null)
check "valid JSON array" jq -e 'type=="array"' >/dev/null <<<"$J"
check "3 running, alpha ram>0, pids numeric" jq -e \
  '(map(select(.state=="running"))|length)==3 and (.[]|select(.name=="alpha")|.ram_mib>0 and (.pids[0]|type=="number"))' >/dev/null <<<"$J"

echo "== stop by name"
run stop alpha
rc_is "stop alpha exits 0" 0
has "prints last pane lines" '^    \| '
has "reports stopped" 'stopped alpha after [0-9]+s'
check "claude process gone" bash -c "! kill -0 $ALPHA 2>/dev/null"
check "tmux session kept" tt has-session -t =alpha
wait_prompt alpha && ok "alpha pane back at a bash prompt" || bad "alpha pane back at a bash prompt"
run stop alpha
has "stopping a stopped session is a no-op" 'not running'

echo "== dry-run and idle filter"
run start alpha cur
rc_is "start alpha cur" 0
declare -A P=()
for n in alpha beta spaced cur; do P[$n]=$(pid_of "$n"); done
run stop --idle 0m --dry-run
has "dry-run lists would-stop" 'would stop beta'
has "dry-run says so" 'dry run: nothing was stopped'
check "dry-run stopped nothing" bash -c "kill -0 ${P[alpha]} && kill -0 ${P[beta]} && kill -0 ${P[spaced]} && kill -0 ${P[cur]}"
run stop --idle 1h --dry-run
has "--idle 1h matches nothing fresh" 'nothing to stop'
run stop --idle 5x
rc_nz "bad duration rejected"

echo "== stop --idle 0m from INSIDE session 'cur' (must protect cur)"
tt new-window -t =cur: -c "$T"
win=$(tt display-message -p -t =cur: '#{pane_id}')   # the new window is active
wait_prompt "$win" || bad "cur window 2 prompt"
cmd=$(printf 'env CS_TMUX_SOCKET=%q CS_TMUX_CONF=/dev/null CS_CONFIG=%q CS_CLAUDE_PROJECTS=%q CS_CLAUDE_BIN=%q CS_STOP_TIMEOUT=5 %q stop --idle 0m > %q 2>&1; echo $? > %q' \
  "$SOCK" "$CS_CONFIG" "$CS_CLAUDE_PROJECTS" "$CS_CLAUDE_BIN" "$CS" "$T/inside.out" "$T/inside.rc")
tt send-keys -t "$win" -l -- "$cmd"; tt send-keys -t "$win" Enter
for _ in $(seq 60); do [[ -s $T/inside.rc ]] && break; sleep 0.5; done
OUT=$(cat "$T/inside.out" 2>/dev/null || true)
has   "cur skipped as current session" 'skipping cur: this is the session you are in'
has   "others stopped" 'stopped beta after'
check "cur claude still alive" alive "${P[cur]}"
check "alpha/beta/spaced gone" bash -c "! kill -0 ${P[alpha]} 2>/dev/null && ! kill -0 ${P[beta]} 2>/dev/null && ! kill -0 ${P[spaced]} 2>/dev/null"
# and ls from inside marks it with '*'
tt send-keys -t "$win" -l -- "${cmd/stop --idle 0m/ls}"; tt send-keys -t "$win" Enter
sleep 1.5; OUT=$(cat "$T/inside.out")
has   "ls marks current session with *" '^\* cur +running'

echo "== restart"
OLD=${P[cur]}
run restart cur
rc_is "restart exits 0" 0
NEW=$(pid_of cur)
check "old claude gone" bash -c "! kill -0 $OLD 2>/dev/null"
check "new claude running" alive "${NEW:-0}"

echo "== detached claude (outside tmux) + stop --pid + ancestor protection"
(cd "$T/p/beta" && env -u TMUX -u TMUX_PANE setsid "$CS_CLAUDE_BIN" </dev/null >/dev/null 2>&1 &)
sleep 1
DET=$(pgrep -n -f -- "/bin/bash $T/bin/claude" || true)
run ls
has "detached claude listed" "pid-$DET +detached .*\(pid $DET, tty \?\)"
run stop --all --dry-run
hasnt "--all never touches detached" "pid-$DET"
run stop --pid $$
rc_nz "--pid on a non-claude refused"; has "refusal message" 'not a claude process'
run stop --pid "$DET"
has "detached stopped by pid" "stopped pid-$DET after"
# a fake claude that runs `cs stop --pid <itself>`: its ancestor is claude
cat > "$T/anc/claude" <<EOF
#!/bin/bash
"$CS" stop --pid \$\$ > "$T/anc.out" 2>&1 || true
EOF
chmod +x "$T/anc/claude"
env -u TMUX -u TMUX_PANE "$T/anc/claude"
OUT=$(cat "$T/anc.out")
has "claude ancestor of the caller is protected" 'skipping pid-[0-9]+: this is the session you are in'

echo "== add / rm"
run add newp "$T/p/with space" --model x
rc_is "add exits 0" 0
check "config has newp line with spaces" grep -qE "^newp +\| $T/p/with space *\| --model x$" "$CS_CONFIG"
run add newp "$T/p/alpha";  rc_nz "duplicate add refused"
run add 'No Good' /tmp;     rc_nz "bad name add refused"
run add other "$T/nope";    rc_nz "missing dir add refused"
run ls; has "newp listed" '^ +newp +no-tmux .*with space'
run start newp
check "args from config passed" grep -qxF "$T/p/with space|--continue --model x" "$LOG"
run rm newp
rc_is "rm exits 0" 0
check "newp gone from config" bash -c "! grep -q '^newp' '$CS_CONFIG'"
check "comments preserved" grep -q '^# test config' "$CS_CONFIG"
run rm newp; rc_nz "rm unknown refused"

echo "== stop --all (from outside: nothing protected on this server)"
run stop --all
rc_is "stop --all exits 0" 0
run ls
has "nothing running afterwards" '^0 claude session\(s\) running'

echo
echo "passed: $PASS   failed: $FAIL"
(( FAIL == 0 ))
