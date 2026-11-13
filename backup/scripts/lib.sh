#!/bin/bash
# shellcheck shell=bash
#
# Shared helpers for every backup script. Each script starts with
#     . /scripts/lib.sh
# Nothing here runs by itself; it only defines settings and functions.
#
# The big picture (RESTORE.md explains it for humans):
#   backup.sh         databases -> /staging (consistent copies), then restic
#                     backs up /staging + the app data directories (/src/...)
#   check.sh          weekly: is the repository intact?
#   restore-test.sh   monthly: can we really get the data back?
#   restore-staging.sh  on request: restore a snapshot to ~/restore-staging/
#   watcher.sh        turns Portal's request files into script runs
#   weekly-summary.sh quiet weekly ntfy message
#
# Bash "strict mode": stop at the first failing command (-e), treat unset
# variables as errors (-u), and let a failure anywhere in a pipeline fail
# the whole pipeline (pipefail).
set -euo pipefail
umask 022

# ---------------------------------------------------------------------------
# Paths inside the container (the mounts are in compose.backup.yml)
# ---------------------------------------------------------------------------
STATE_DIR=/state                      # host ./data/backup (Portal reads it)
STATUS_FILE=$STATE_DIR/status.json    # the Portal contract (PLAN-2.md)
REQUEST_DIR=$STATE_DIR/requests       # Portal drops request files here
TEXTFILE=/textfile/backup.prom        # host ./run/textfile -> node_exporter
STAGING=/staging                      # consistent DB copies + dumps (docker volume)
WORK=/work                            # restic cache, temp files, logs (docker volume)
RESTORE_STAGING=/restore-staging      # host ~/restore-staging (ONLY restore target)
LOCK_FILE=/tmp/backup.lock            # one job at a time (see acquire_lock)
HOST_TAG=experimentos                 # restic --host for every snapshot

# Who owns the files we hand to the host (status.json, metrics, restores).
OWNER_UID=${PUID:-1000}
OWNER_GID=${PGID:-1000}
# They end up in setpriv --reuid/--regid: digits only (never root by accident).
[[ $OWNER_UID =~ ^[1-9][0-9]{0,9}$ && $OWNER_GID =~ ^[1-9][0-9]{0,9}$ ]] ||
  { echo "PUID/PGID must be non-zero numbers" >&2; exit 1; }

NTFY_URL=${NTFY_URL:-http://ntfy:80}
NTFY_TOPIC=backups

# ---------------------------------------------------------------------------
# What gets backed up
# ---------------------------------------------------------------------------
# Every live SQLite database, relative to /src. Each one is copied with
# sqlite3 ".backup" into /staging/sqlite/<same path> (see backup.sh). The raw
# files are excluded from the restic file walk (backup/excludes.txt).
SQLITE_DBS=(
  platform/portal.db           # Portal: users, sessions (WAL)
  platform/ntfy/cache.db       # ntfy: message cache
  platform/ntfy/auth.db        # ntfy: users, tokens, access rules
  focal/focal.db               # Focal (rollback journal)
  ritual/ritual.db             # Ritual (WAL, big -wal file)
  ledger/ledger.db             # Ledger (WAL)
  gitea/gitea/gitea.db         # Gitea metadata (users, issues, PRs...)
  dsa-tracker/dsa_tracker.db   # DSA tracker (WAL)
  ramayana-imax-bot/state.db   # Ramayana IMAX bot state
)
# Database-looking files (by name) that are deliberately NOT databases we
# care about. The safety net in backup.sh refuses anything unknown, so a
# false alarm is fixed by adding the path here (relative to /src).
SQLITE_IGNORE=()

# Directories that restic walks file by file (minus excludes.txt). The
# safety net searches exactly these for database files nobody listed.
WALKED_TREES=(/src/platform /src/focal /src/ritual /src/ledger /src/gitea)

# Postgres: "container database user". pg_dump runs INSIDE the container via
# "docker exec" through the backup socket proxy (compose.backup.yml).
PG_DBS=(
  "dalal-postgres-1 dalal dalal"
  "conveyor-postgres-1 conveyor conveyor"
)

# ---------------------------------------------------------------------------
# Logging
# ---------------------------------------------------------------------------
log() { printf '%s [%s] %s\n' "$(date '+%Y-%m-%d %H:%M:%S %Z')" "${JOB:-backup}" "$*" >&2; }
die() { log "ERROR: $*"; exit 1; }

# ---------------------------------------------------------------------------
# Running a command as another user
# ---------------------------------------------------------------------------
# The container runs as root, but without most of root's powers (cap_drop
# ALL in compose.backup.yml). Writing into directories that belong to uid
# 1000 would need CAP_DAC_OVERRIDE, which we deliberately do NOT have. So
# whatever must land in a uid-1000 directory is written BY uid 1000:
#   as_ids UID GID command...   (setpriv needs CAP_SETUID + CAP_SETGID)
#
# The child gets a FRESH, minimal environment (env -i): our environment holds
# RESTIC_PASSWORD, NTFY_TOKEN_BACKUP, RCLONE_CONFIG, DOCKER_HOST... and the
# children (sqlite3, tar unpacking a snapshot, sh, rm) run as other uids and
# do not need any of it. An allowlist, so a secret added later is covered too.
AS_IDS_PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
as_ids() {
  local uid=$1 gid=$2
  shift 2
  [[ $uid =~ ^[0-9]{1,10}$ && $gid =~ ^[0-9]{1,10}$ ]] || die "as_ids: bad uid/gid '$uid' '$gid'"
  env -i PATH="$AS_IDS_PATH" HOME=/nonexistent TMPDIR=/tmp TZ="${TZ:-UTC}" \
    setpriv --reuid="$uid" --regid="$gid" --clear-groups -- "$@"
}
as_owner() { as_ids "$OWNER_UID" "$OWNER_GID" "$@"; }

# sqlite3 with no ~/.sqliterc, no prompts, stop at the first error.
sqlite() { sqlite3 -batch -bail -init /dev/null "$@"; }

# Does this file start with the 16-byte SQLite magic "SQLite format 3\0"?
is_sqlite_file() {
  [ "$(head -c 16 -- "$1" 2>/dev/null | tr '\0' '@')" = "SQLite format 3@" ]
}

# PRAGMA integrity_check on every SQLite file below DIR (found by content,
# not by name), as the owner of each file. Prints "ok <path>" or
# "FAIL <path>: <message>" per file and sets SQLITE_CHECKED / SQLITE_FAILED.
check_sqlite_tree() {
  local dir=$1 f res uid gid
  SQLITE_CHECKED=0
  SQLITE_FAILED=0
  while IFS= read -r -d '' f; do
    is_sqlite_file "$f" || continue
    read -r uid gid < <(stat -c '%u %g' -- "$f")
    res=$(as_ids "$uid" "$gid" sqlite3 -batch -init /dev/null "$f" "PRAGMA integrity_check;" 2>&1 | head -c 500 || true)
    SQLITE_CHECKED=$((SQLITE_CHECKED + 1))
    if [ "$res" = "ok" ]; then
      echo "ok   ${f#"$dir"/}"
    else
      SQLITE_FAILED=$((SQLITE_FAILED + 1))
      echo "FAIL ${f#"$dir"/}: $(printf '%s' "$res" | tr '\n' ' ')"
    fi
  done < <(find "$dir" -type f -size +0 -print0 | sort -z)
}

# ---------------------------------------------------------------------------
# Which repository? (BACKUP_TARGET in .env)
# ---------------------------------------------------------------------------
# local  -> /repo-local = host /var/backups/restic-local (same disk: protects
#           against mistakes and corruption, NOT against a dead disk or theft)
# drive  -> Google Drive through rclone (off-site copy)
select_repo() {
  [ -n "${RESTIC_PASSWORD:-}" ] || die "RESTIC_PASSWORD is empty (it must be set in ~/platform/.env)"
  case "${BACKUP_TARGET:-}" in
    local)
      mountpoint -q /repo-local || die "/repo-local is not a mount; refusing to write a repository into the container"
      export RESTIC_REPOSITORY=/repo-local
      ;;
    drive)
      export RESTIC_REPOSITORY=${BACKUP_DRIVE_REPO:-rclone:gdrive:experimentos-restic}
      prepare_rclone
      ;;
    *)
      die "BACKUP_TARGET must be 'local' or 'drive', got '${BACKUP_TARGET:-}'"
      ;;
  esac
  export RESTIC_CACHE_DIR=$WORK/cache
}

# rclone must be able to SAVE its config: Google access tokens expire after
# an hour and rclone writes the refreshed one back. The host file
# secrets/rclone.conf is mounted read-only (root here cannot write a uid-1000
# file without CAP_DAC_OVERRIDE), so rclone works on a copy in the work
# volume. The copy is refreshed whenever the host file is newer (e.g. after
# backup/drive-setup.sh ran again).
prepare_rclone() {
  local src=/secrets/rclone.conf dst=$WORK/rclone/rclone.conf
  [ -s "$src" ] || die "secrets/rclone.conf is empty: run backup/drive-setup.sh first (RESTORE.md, 'Google Drive')"
  mkdir -p "$WORK/rclone"
  chmod 700 "$WORK/rclone"
  if [ ! -s "$dst" ] || [ "$src" -nt "$dst" ]; then
    install -m 600 "$src" "$dst"
    log "rclone: using the new secrets/rclone.conf"
  fi
  grep -q '^\[gdrive\]' "$dst" || die "secrets/rclone.conf has no [gdrive] section"
  export RCLONE_CONFIG=$dst
}

# Open the repository; create it ("restic init") the very first time.
# restic exit codes: 10 = repository does not exist, 12 = wrong password.
# Only "does not exist" leads to init, never "cannot read" or "wrong password".
ensure_repo() {
  local rc=0 err=$WORK/tmp/cat-config.err
  restic cat config >/dev/null 2>"$err" || rc=$?
  case $rc in
    0) ;;
    10)
      log "repository $RESTIC_REPOSITORY does not exist yet: restic init"
      restic init
      ;;
    *)
      cat "$err" >&2
      die "cannot open repository $RESTIC_REPOSITORY (restic exit $rc; 12 = wrong RESTIC_PASSWORD)"
      ;;
  esac
}

# Same, but never create anything (check / restore jobs).
require_repo() {
  local rc=0 err=$WORK/tmp/cat-config.err
  restic cat config >/dev/null 2>"$err" || rc=$?
  if [ "$rc" -ne 0 ]; then
    cat "$err" >&2
    die "cannot open repository $RESTIC_REPOSITORY (restic exit $rc; 10 = not initialised, 12 = wrong RESTIC_PASSWORD)"
  fi
}

# ---------------------------------------------------------------------------
# One job at a time
# ---------------------------------------------------------------------------
# backup, check, restore-test and restore-staging all take this lock, so
# they never overlap (e.g. a "Back up now" click during the nightly run just
# waits). The lock is released automatically when the process exits.
acquire_lock() {
  exec 9>>"$LOCK_FILE"
  if ! flock -n 9; then
    log "another backup job is running; waiting for it (at most ${LOCK_WAIT:-7200}s)"
    flock -w "${LOCK_WAIT:-7200}" 9 || die "gave up waiting for the lock"
  fi
}

# ---------------------------------------------------------------------------
# Atomic files for the host: status.json and backup.prom
# ---------------------------------------------------------------------------
# Readers (Portal, node_exporter) must never see a half-written file, so we
# write a temporary file in the same directory and rename it over the old
# one: rename() is atomic. Written as uid 1000 (see as_ids) with mode 644:
# Portal (uid 1000) and node_exporter (nobody) can both read it.
write_file_as_owner() {
  local dest=$1 content=$2 tmp
  tmp="$(dirname "$dest")/.$(basename "$dest").tmp.$$"
  printf '%s\n' "$content" |
    as_owner sh -c 'umask 022 && cat > "$1" && chmod 644 "$1" && mv -f "$1" "$2"' sh "$tmp" "$dest"
}

# jq helper: restic prints times like 2026-11-13T03:00:01.123+05:30; jq's
# fromdate only understands "Z", so convert by hand to unix seconds.
JQ_TS='def ts: capture("^(?<d>[0-9-]+T[0-9:]+)(\\.[0-9]+)?(?<z>Z|(?<s>[+-])(?<h>[0-9]{2}):(?<m>[0-9]{2}))$")
  | ((.d + "Z") | fromdateiso8601)
    - (if .z == "Z" then 0 else (((.h | tonumber) * 3600 + (.m | tonumber) * 60) * (if .s == "+" then 1 else -1 end)) end);'

# The empty status (schema: PLAN-2.md, Phase C "Status for Portal").
# null = "never happened yet".
status_default() {
  jq -n '{target: "", running: "", last_run: null, last_success: null,
          snapshot_count: 0, repo_size_bytes: 0, snapshots: [],
          last_check: null, last_restore_test: null, last_staging_restore: null}'
}

# status.json is UNTRUSTED input: it lives in a directory that uid 1000 (and
# the Portal container) can write. A value like "a[$(cmd)]" that reached
# bash arithmetic ($(( )), (( )), [[ x -gt y ]]) would run cmd as root in
# this container. So every read goes through this jq filter, which rebuilds
# the object from the schema (PLAN-2.md): numbers stay non-negative integers
# or become null, booleans stay booleans or become null, strings are
# length-capped, snapshot ids must be hex, unknown keys are dropped. Since
# status_update writes what status_read returns, a poisoned file is
# normalised on the next write. Shell code still validates numbers again
# (status_num) before any arithmetic: defence in depth.
JQ_SANITIZE='
  def num: if type == "number" and . >= 0 and . < 1e15 then floor else null end;
  def str($n): if type == "string" then .[:$n] else "" end;
  def bool: if type == "boolean" then . else null end;
  def obj(f): if type == "object" then f else null end;
  def sanitize: {
    target: (.target | str(16)),
    running: (.running | if IN("backup", "restore-staging", "check", "restore-test") then . else "" end),
    last_run: (.last_run | obj({
      started: (.started | num), finished: (.finished | num),
      status: (.status | if IN("ok", "failed") then . else "failed" end),
      duration_s: (.duration_s | num), bytes_added: (.bytes_added | num),
      error: (.error | str(5000))})),
    last_success: (.last_success | num),
    snapshot_count: ((.snapshot_count | num) // 0),
    repo_size_bytes: ((.repo_size_bytes | num) // 0),
    snapshots: (.snapshots | if type == "array" then
        [ .[] | objects | {id: (.id | if type == "string" and test("\\A[0-9a-f]{8,64}\\z") then . else null end),
                           time: (.time | num)} | select(.id != null) ] | .[:10]
      else [] end),
    last_check: (.last_check | obj({time: (.time | num), ok: (.ok | bool)})),
    last_restore_test: (.last_restore_test | obj({time: (.time | num), ok: (.ok | bool),
      files_checked: (.files_checked | num), detail: (.detail | str(1000))})),
    last_staging_restore: (.last_staging_restore | obj({time: (.time | num), ok: (.ok | bool),
      snapshot: (.snapshot | str(64)), path: (.path | str(512)), detail: (.detail | str(1000))}))
  };'

# Current status: defaults overlaid with whatever the file has (so fields
# we do not touch are preserved, e.g. last_success after a failed run),
# then sanitised (see JQ_SANITIZE).
status_read() {
  if [ -s "$STATUS_FILE" ] && head -c 1048576 "$STATUS_FILE" | jq -e 'type == "object"' >/dev/null 2>&1; then
    jq -s "$JQ_SANITIZE"' .[0] * .[1] | sanitize' <(status_default) <(head -c 1048576 "$STATUS_FILE")
  else
    status_default
  fi
}

# is_uint VALUE: a plain decimal number, no sign, no leading zeros (bash
# would read 010 as octal), at most 18 digits. Use it on EVERY value that
# comes from a file or a program before it reaches arithmetic.
is_uint() { [[ ${1-} =~ ^(0|[1-9][0-9]{0,17})$ ]]; }

# status_num JQ_PATH: prints the number at that path of status.json, or
# NOTHING if it is missing or not a plain non-negative integer.
#   last=$(status_num .last_success)
status_num() {
  local v
  v=$(status_read | jq -r "($1) // empty" 2>/dev/null) || v=""
  if is_uint "$v"; then printf '%s' "$v"; fi
  return 0
}

# status_update [jq options...] FILTER
# Applies a jq filter to the status and writes it back, then regenerates the
# metrics file from it (status.json is the single source of truth).
# Example: status_update --arg r backup '.running = $r'
status_update() {
  local filter=${*: -1} new
  local opts=("${@:1:$#-1}")
  new=$(status_read | jq "${opts[@]}" --arg target "${BACKUP_TARGET:-}" ".target = \$target | $filter") || {
    log "could not update status.json"
    return 1
  }
  write_file_as_owner "$STATUS_FILE" "$new"
  write_metrics
}

# node_exporter's textfile collector turns this file into metrics
# (names: PLAN-2.md). backup_last_success_timestamp_seconds is the
# "dead man's switch": the BackupStale alert fires when it is older than 26h
# or missing, so a backup that silently stopped running is noticed too.
# Metrics for things that never happened are left out (absent).
write_metrics() {
  local text
  text=$(status_read | jq -r '
    def m(name; help; v):
      if v == null then empty
      else "# HELP \(name) \(help)\n# TYPE \(name) gauge\n\(name) \(v)" end;
    def b(x): if x == null then null elif x then 1 else 0 end;
    [ m("backup_last_success_timestamp_seconds"; "Unix time the last successful backup finished."; .last_success),
      m("backup_last_run_timestamp_seconds"; "Unix time the last backup run finished (ok or not)."; .last_run.finished),
      m("backup_last_run_status"; "1 if the last backup run succeeded, 0 if it failed."; (if .last_run == null then null elif .last_run.status == "ok" then 1 else 0 end)),
      m("backup_last_duration_seconds"; "Duration of the last backup run."; .last_run.duration_s),
      m("backup_last_bytes_added"; "Bytes the last backup added to the repository (after dedup and compression)."; .last_run.bytes_added),
      m("backup_snapshot_count"; "Snapshots in the repository."; .snapshot_count),
      m("backup_repo_size_bytes"; "Repository size on storage (restic stats raw-data)."; .repo_size_bytes),
      m("backup_check_last_timestamp_seconds"; "Unix time of the last restic check."; .last_check.time),
      m("backup_check_last_status"; "1 if the last restic check passed, 0 if not."; b(.last_check.ok)),
      m("backup_restore_test_last_timestamp_seconds"; "Unix time of the last restore test."; .last_restore_test.time),
      m("backup_restore_test_last_status"; "1 if the last restore test passed, 0 if not."; b(.last_restore_test.ok))
    ] | join("\n")')
  write_file_as_owner "$TEXTFILE" "$text"
}

# Refresh snapshot list / count / repo size in status.json.
refresh_repo_info() {
  local snaps=$WORK/tmp/snapshots.json size
  restic snapshots --json --no-lock >"$snaps"
  size=$(restic stats --mode raw-data --json --no-lock | jq '.total_size')
  is_uint "$size" || die "restic stats printed no usable total_size"
  status_update --slurpfile s "$snaps" --argjson z "$size" "$JQ_TS"'
    .snapshot_count = ($s[0] | length)
    | .repo_size_bytes = $z
    | .snapshots = ($s[0] | map({id: .id, time: (.time | ts)}) | sort_by(.time) | reverse | .[:10])'
}

# ---------------------------------------------------------------------------
# Notifications (ntfy topic "backups")
# ---------------------------------------------------------------------------
# notify PRIORITY TITLE MESSAGE [TAGS]
# Never fails: a broken ntfy must not turn a good backup into a failed one.
# The token is passed to curl on stdin (--config -), not on the command
# line, so it does not show up in "ps".
notify() {
  local prio=$1 title=$2 msg=$3 tags=${4:-floppy_disk} body
  if [ -z "${NTFY_TOKEN_BACKUP:-}" ]; then
    log "ntfy: NTFY_TOKEN_BACKUP is empty, not sending '$title'"
    return 0
  fi
  # Headers: fixed values from our own scripts, but never let a CR/LF in.
  [[ $prio =~ ^[1-5]$ ]] || prio=5
  title=${title//[$'\r\n']/ }
  tags=${tags//[^a-z0-9_,]/}
  # The body goes through a file: "--data-binary <text>" would treat text
  # that starts with "@" as a FILE NAME to upload.
  body=$(mktemp /tmp/ntfy-body.XXXXXX) || { log "ntfy: no temp file, not sending '$title'"; return 0; }
  printf '%s' "$msg" | head -c 3500 >"$body"
  if printf 'header = "Authorization: Bearer %s"\n' "$NTFY_TOKEN_BACKUP" |
    curl -fsS -m 15 --retry 2 -o /dev/null --config - \
      -H "Title: $title" -H "Priority: $prio" -H "Tags: $tags" \
      --data-binary "@$body" "$NTFY_URL/$NTFY_TOPIC"; then
    log "ntfy: sent '$title' (priority $prio)"
  else
    log "ntfy: could not publish '$title' (ignored)"
  fi
  rm -f -- "$body"
  return 0
}

# "2026-11-13 03:00 IST" from unix seconds (container TZ is Asia/Kolkata).
# Anything that is not a plain number (missing, null, garbage) is "never".
fmt_time() { if is_uint "${1-}"; then date -d "@$1" '+%Y-%m-%d %H:%M %Z'; else echo never; fi; }
# 1536 -> "1.5 KiB" (non-numbers count as 0)
fmt_bytes() {
  local b=${1-}
  is_uint "$b" || b=0
  awk -v b="$b" 'BEGIN{split("B KiB MiB GiB TiB",u," ");i=1;while(b>=1024&&i<5){b/=1024;i++};printf (i==1?"%d %s":"%.1f %s"),b,u[i]}'
}

# ---------------------------------------------------------------------------
# Job runner: lock, "running" flag, log capture, success/failure handling
# ---------------------------------------------------------------------------
# A script defines:
#   job_main ARGS...   the work; runs in a subshell with strict mode. Set
#                      STEP="..." before each step: on failure the message
#                      says which step broke.
#   job_success        called after job_main succeeded
#   job_failure        called after job_main failed (JOB_ERROR = log tail)
# and then calls:  run_job <name> ARGS...
# <name> is what status.json "running" shows: backup | check |
# restore-test | restore-staging.
run_job() {
  local name=$1 rc=0
  shift
  JOB=$name
  mkdir -p "$WORK/logs" "$WORK/tmp"
  RUNLOG=$WORK/logs/$name.log
  acquire_lock
  JOB_STARTED=$(date +%s)
  # Clear the flag however we leave (also on errors in the handlers).
  trap 'status_update ".running = \"\"" || true' EXIT
  trap 'log "interrupted"; exit 130' INT TERM
  status_update --arg r "$name" '.running = $r'

  # The work runs in a subshell piped into tee: everything it prints goes to
  # the container log AND to $RUNLOG, whose last lines become the error
  # message. The ERR trap names the failing step. (set -E: the trap also
  # fires inside functions.)
  set +e
  (
    set -Eeuo pipefail
    STEP=start
    trap 'echo "FAILED in step \"$STEP\" (exit $?)" >&2' ERR
    job_main "$@"
  ) 2>&1 | tee "$RUNLOG"
  rc=${PIPESTATUS[0]}
  set -e

  JOB_FINISHED=$(date +%s)
  JOB_DURATION=$((JOB_FINISHED - JOB_STARTED))
  if [ "$rc" -eq 0 ]; then
    log "finished OK in ${JOB_DURATION}s"
    job_success
  else
    JOB_ERROR=$(grep -v '^\s*$' "$RUNLOG" | tail -n 15 | cut -c1-300 || true)
    log "FAILED (exit $rc) after ${JOB_DURATION}s"
    job_failure
  fi
  return "$rc"
}
