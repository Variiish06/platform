#!/bin/bash
# Nightly (and on-demand) backup.
#
#   /scripts/backup.sh nightly    from the crontab
#   /scripts/backup.sh manual     "Back up now" in Portal, or by hand:
#       docker compose exec backup /scripts/backup.sh manual
#
# Steps (each one fails the whole run if it fails):
#   1. empty /staging
#   2. copy every SQLite database with sqlite3 ".backup" (consistent, online)
#      and run PRAGMA integrity_check on the copy
#   3. pg_dump every Postgres database (plain SQL)
#   4. restic backup /staging + the app data directories
#   5. restic forget --prune (retention: 7 daily, 4 weekly, 6 monthly)
#   6. status.json + metrics; on failure an URGENT ntfy message
#
# Why not just copy the .db files? A live SQLite database is a file that is
# being changed in place. A plain copy can catch it half-way through a write
# ("torn page") and, in WAL mode, the newest data is not even in the .db file
# but in the separate -wal file. sqlite3's online backup reads the database
# through SQLite itself, under a read lock, so the copy is a consistent
# snapshot including the WAL content. See RESTORE.md, "Why not copy the files?".
. /scripts/lib.sh

MODE=${1:-manual}
case "$MODE" in
  nightly | manual) ;;
  *) die "usage: backup.sh nightly|manual" ;;
esac

# --- step 2: SQLite --------------------------------------------------------
# Copy one database. $1 = path relative to /src, e.g. "ritual/ritual.db".
stage_sqlite_db() {
  local rel=$1 src=/src/$1 dest=$STAGING/sqlite/$1 uid gid res
  # Paths go into sqlite3 dot-commands; allow only boring characters.
  [[ $rel =~ ^[A-Za-z0-9._/-]+$ ]] || die "unsafe database path: $rel"
  if [ ! -f "$src" ] || [ -L "$src" ]; then
    die "database missing: $src (remove it from SQLITE_DBS in lib.sh if it is gone for good)"
  fi
  is_sqlite_file "$src" || die "$src is not an SQLite database"

  # Run sqlite3 AS THE OWNER OF THE DATABASE. Opening a database can create
  # its -wal/-shm/-journal companions; if we did that as root, the app
  # (running as that owner) could no longer open its own database.
  read -r uid gid < <(stat -c '%u %g' -- "$src")
  as_ids "$uid" "$gid" mkdir -p "$(dirname "$dest")"
  # file:...?mode=rw: never CREATE a database if it vanished meanwhile.
  # .timeout 10000: wait up to 10 s if the app holds a write lock.
  as_ids "$uid" "$gid" sqlite3 -batch -bail -init /dev/null "file:$src?mode=rw" \
    ".timeout 10000" ".backup '$dest'"

  # Is the copy a healthy database? integrity_check prints exactly "ok".
  res=$(as_ids "$uid" "$gid" sqlite3 -batch -init /dev/null "$dest" "PRAGMA integrity_check;" 2>&1 || true)
  [ "$res" = "ok" ] || die "integrity_check of the copy of $rel failed: $(printf '%s' "$res" | head -c 300)"
  log "  sqlite  $rel  $(fmt_bytes "$(stat -c %s "$dest")")  integrity_check: ok"
  SQLITE_SUMMARY+="  $rel  $(stat -c %s "$dest") bytes  integrity_check ok"$'\n'
}

stage_sqlite() {
  local rel f known=" " extra=()
  for rel in "${SQLITE_DBS[@]}" "${SQLITE_IGNORE[@]}"; do known+="$rel "; done

  # Safety net: look for database-looking files nobody listed. Raw .db files
  # are excluded from the file walk (excludes.txt), so an unlisted database
  # would otherwise be skipped SILENTLY. Unlisted SQLite files are copied
  # anyway (with a warning); anything else named like a database fails the
  # run so a human looks at it.
  while IFS= read -r -d '' f; do
    rel=${f#/src/}
    case "$known" in *" $rel "*) continue ;; esac
    if is_sqlite_file "$f"; then
      log "  WARNING: database not in SQLITE_DBS (lib.sh): $rel  -> copied anyway; please add it to the list"
      extra+=("$rel")
    else
      die "$f is named like a database but is not SQLite; add it to SQLITE_IGNORE in lib.sh if that is fine"
    fi
  done < <(find "${WALKED_TREES[@]}" -type f \( -name '*.db' -o -name '*.sqlite' -o -name '*.sqlite3' \) -print0 | sort -z)

  for rel in "${SQLITE_DBS[@]}" "${extra[@]}"; do
    stage_sqlite_db "$rel"
  done
}

# --- step 3: Postgres ------------------------------------------------------
# pg_dump runs inside the database container ("docker exec"), talking to
# the server over its local socket; we only receive the SQL text. Plain,
# uncompressed SQL on purpose: restic deduplicates unchanged parts between
# nights (a compressed dump changes completely when one row changes) and
# compresses the repository itself.
stage_postgres() {
  local spec container db user out
  for spec in "${PG_DBS[@]}"; do
    read -r container db user <<<"$spec"
    out=$STAGING/postgres/$db.sql
    # A database stopped on purpose (e.g. from the Portal) cannot be dumped,
    # and its data has not changed since it stopped. Carry the previous
    # snapshot's dump forward instead of failing the whole backup, so the
    # dump stays in every new snapshot and retention never drops it.
    if [ "$(docker inspect --format '{{.State.Running}}' "$container" 2>/dev/null)" != true ]; then
      restic dump --host "$HOST_TAG" latest "/staging/postgres/$db.sql" >"$out" 2>/dev/null ||
        die "$container is not running and no earlier dump of $db exists to carry forward (start it, or remove it from PG_DBS)"
      tail -c 300 "$out" | grep -q 'PostgreSQL database dump complete' ||
        die "$container is not running and the earlier dump of $db is incomplete"
      log "  postgres  $container/$db  $(fmt_bytes "$(stat -c %s "$out")")  container stopped: previous dump carried forward"
      PG_SUMMARY+="  $db.sql  CARRIED FORWARD from the previous snapshot: container $container was stopped  $(stat -c %s "$out") bytes"$'\n'
      continue
    fi
    docker exec "$container" pg_dump --username="$user" --dbname="$db" --no-password >"$out"
    [ -s "$out" ] || die "pg_dump of $db produced an empty file"
    # pg_dump writes this marker as its very last line; a dump cut short
    # (container restarted, disk full...) does not have it.
    tail -c 300 "$out" | grep -q 'PostgreSQL database dump complete' ||
      die "pg_dump of $db is incomplete (no 'database dump complete' marker)"
    log "  postgres  $container/$db  $(fmt_bytes "$(stat -c %s "$out")")  dump complete"
    PG_SUMMARY+="  $db.sql  from container $container (database $db, user $user)  $(stat -c %s "$out") bytes"$'\n'
  done
}

# A small README inside every snapshot, for whoever restores it one day.
write_manifest() {
  cat >"$STAGING/MANIFEST.txt" <<EOF
Backup of $HOST_TAG, taken $(date '+%Y-%m-%d %H:%M %Z') ($(date -u '+%Y-%m-%d %H:%M UTC')), mode $MODE.
Tools: $(restic version | head -n1); sqlite $(sqlite3 --version | cut -d' ' -f1)
How to restore: RESTORE.md in the platform repository (~/platform/RESTORE.md).

Layout of this snapshot
  /staging/sqlite/<app>/...   consistent copies of the live SQLite databases
                              (USE THESE, the raw .db files are not in the backup)
  /staging/postgres/<db>.sql  pg_dump plain SQL dumps
  /src/<app>/...              the app data directories, minus live databases,
                              caches and logs (backup/excludes.txt)

Where things live on the server (/src/<app> = host path)
  /src/platform           ~/platform/data
  /src/focal              ~/focal/data
  /src/ritual             ~/apps/ritual/data
  /src/ledger             ~/apps/ledger/data
  /src/gitea              ~/gitea/data           (ssh/ holds root-owned host keys)
  /src/dsa-tracker        ~/dsa-tracker          (only the database is backed up)
  /src/ramayana-imax-bot  ~/ramayana-imax-bot    (only the database is backed up)

SQLite copies (sqlite3 .backup, then PRAGMA integrity_check)
${SQLITE_SUMMARY}
Postgres dumps
${PG_SUMMARY}
EOF
}

job_main() {
  local out=$WORK/tmp/backup-out.json summary free_kb
  SQLITE_SUMMARY=""
  PG_SUMMARY=""

  STEP="select repository"
  select_repo
  log "backup ($MODE) to $RESTIC_REPOSITORY  [$(restic version | head -n1)]"
  free_kb=$(df -Pk "$STAGING" | awk 'NR == 2 {print $4}')
  [ "$free_kb" -gt 1048576 ] || die "less than 1 GiB free for /staging"

  STEP="open repository"
  ensure_repo
  restic unlock >/dev/null 2>&1 || true # removes only STALE locks (e.g. after a crash)

  STEP="clean staging"
  # Copies in /staging/sqlite belong to the database owners: each owner
  # removes its own (root may not delete in their directories, by design).
  mkdir -p "$STAGING/sqlite" "$STAGING/postgres"
  local e
  for e in "$STAGING"/sqlite/* "$STAGING"/sqlite/.[!.]*; do
    [ -e "$e" ] || [ -L "$e" ] || continue
    as_ids "$(stat -c %u -- "$e")" "$(stat -c %g -- "$e")" rm -rf -- "$e"
  done
  rm -rf "${STAGING:?}/postgres" "$STAGING/MANIFEST.txt"
  mkdir -p "$STAGING/postgres"
  chmod 755 "$STAGING"
  chmod 1777 "$STAGING/sqlite" # any owner may create its own copy here (sticky: only its own)
  chmod 700 "$STAGING/postgres"

  STEP="copy SQLite databases"
  log "copying SQLite databases"
  stage_sqlite

  STEP="dump Postgres databases"
  log "dumping Postgres databases"
  stage_postgres

  STEP="write manifest"
  write_manifest

  STEP="restic backup"
  # --json: machine-readable progress; we only keep the final "summary".
  # Exit code 3 ("some files could not be read") is a failure for us too.
  local rc=0
  restic backup --json --retry-lock 5m \
    --host "$HOST_TAG" --tag "$MODE" \
    --exclude-file /etc/backup/excludes.txt --exclude-caches \
    "$STAGING" "${WALKED_TREES[@]}" >"$out" || rc=$?
  jq -r 'select(.message_type == "error") | "restic error: \(.during // "") \(.item // "") \(.error.message // .error // "")"' "$out" >&2 || true
  [ "$rc" -eq 0 ] || die "restic backup exited with $rc"
  summary=$(jq -c 'select(.message_type == "summary")' "$out" | tail -n1)
  [ -n "$summary" ] || die "restic backup printed no summary"
  jq -r '"snapshot \(.snapshot_id[:8]): \(.files_new) new, \(.files_changed) changed, \(.files_unmodified) unchanged files; \(.total_bytes_processed) bytes scanned, \(.data_added) bytes new before compression, \(.data_added_packed // .data_added) bytes stored"' <<<"$summary" >&2
  # Bytes the repository really grew by (after dedup + compression).
  jq -r '.data_added_packed // .data_added' <<<"$summary" >"$WORK/tmp/bytes_added"

  STEP="forget/prune"
  # Keep the newest snapshot of each of the last 7 days, 4 weeks, 6 months;
  # --prune then deletes data no remaining snapshot needs.
  restic forget --retry-lock 5m --host "$HOST_TAG" \
    --keep-daily 7 --keep-weekly 4 --keep-monthly 6 --prune 2>&1 |
    sed '/^[[:space:]]*$/d' | tail -n 20 >&2

  STEP="read repository info"
  refresh_repo_info
}

job_success() {
  local added
  added=$(cat "$WORK/tmp/bytes_added" 2>/dev/null || echo 0)
  is_uint "$added" || added=0 # restic output; goes into JSON and history.txt
  status_update --argjson s "$JOB_STARTED" --argjson f "$JOB_FINISHED" --argjson d "$JOB_DURATION" --argjson b "${added:-0}" '
    .last_run = {started: $s, finished: $f, status: "ok", duration_s: $d, bytes_added: $b, error: ""}
    | .last_success = $f'
  # Keep a tiny history for the weekly summary ("bytes added this week").
  echo "$JOB_FINISHED $added" >>"$WORK/history.txt"
  tail -n 400 "$WORK/history.txt" >"$WORK/history.tmp" && mv "$WORK/history.tmp" "$WORK/history.txt"
  # Success is silent ON PURPOSE: a daily "all good" message trains you to
  # ignore the topic. Silence + the weekly summary + the BackupStale alert
  # (fires if backup_last_success_timestamp_seconds gets older than 26 h)
  # cover the good case; failures are loud.
}

job_failure() {
  # last_success is NOT touched: it still says when the last GOOD backup was.
  status_update --argjson s "$JOB_STARTED" --argjson f "$JOB_FINISHED" --argjson d "$JOB_DURATION" --arg e "$JOB_ERROR" '
    .last_run = {started: $s, finished: $f, status: "failed", duration_s: $d, bytes_added: 0, error: $e}'
  notify 5 "Backup FAILED ($MODE)" "Backup on $HOST_TAG failed at $(fmt_time "$JOB_FINISHED").
Last successful backup: $(fmt_time "$(status_num .last_success)").

$JOB_ERROR

Log: docker compose logs backup (in ~/platform)" "rotating_light,floppy_disk"
}

run_job backup "$@"
