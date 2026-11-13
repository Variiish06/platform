#!/bin/bash
# Monthly restore test (crontab: the 1st, 04:30 IST).
#
#   docker compose exec backup /scripts/restore-test.sh
#
# A backup you have never restored is a hope, not a backup. Things that
# only show up when you restore: a wrong password, a repository with
# missing data, databases that were copied while half-written, dumps that
# stop in the middle, files you thought were included but were excluded.
# So once a month we restore the newest snapshot for real (into a scratch
# directory in the container's work volume, NOT ~/restore-staging), and:
#   - run PRAGMA integrity_check on every SQLite file in it (found by
#     content; zero files counts as a failure),
#   - check that every database in SQLITE_DBS is there,
#   - check that every Postgres dump is there, non-empty and complete.
# The scratch directory is always deleted afterwards.
. /scripts/lib.sh

TEST_ROOT=$WORK/restore-test

job_main() {
  local snap spec container db user f missing=0 old
  rm -f "$WORK/tmp/restore-test.files" "$WORK/tmp/restore-test.detail"
  STEP="select repository"
  select_repo
  require_repo

  STEP="find latest snapshot"
  snap=$(restic snapshots --json --no-lock --host "$HOST_TAG" --latest 1 | jq -r 'sort_by(.time) | last | .id // empty')
  [[ $snap =~ ^[0-9a-f]{64}$ ]] || die "the repository has no snapshot for host $HOST_TAG"

  STEP="restore"
  mkdir -p "$TEST_ROOT"
  chmod 1777 "$TEST_ROOT"
  # Scratch copies left by an interrupted earlier test.
  for old in "$TEST_ROOT"/test-*; do
    [ -d "$old" ] && as_owner sh -c 'chmod -R u+rwX "$1"; rm -rf -- "$1"' sh "$old"
  done
  # Global (not "local"): the EXIT trap runs after job_main has returned.
  dir=$TEST_ROOT/test-$(date -u +%Y%m%d-%H%M%S)
  as_owner mkdir -m 700 "$dir"
  # Always remove the scratch copy, whatever happens below.
  trap 'as_owner chmod -R u+rwX "${dir:?}" 2>/dev/null; as_owner rm -rf -- "${dir:?}"' EXIT
  log "restoring snapshot ${snap:0:8} into a scratch directory"
  # Same mechanism as restore-staging.sh: restic streams the snapshot as a
  # tar archive, unpacked by uid $OWNER_UID (files end up owned by it).
  restic dump --no-lock --archive tar "$snap" / | as_owner tar -x -f - -C "$dir"

  STEP="check SQLite files"
  check_sqlite_tree "$dir" >&2
  [ "$SQLITE_CHECKED" -gt 0 ] || die "no SQLite files found in the restored snapshot"
  [ "$SQLITE_FAILED" -eq 0 ] || die "$SQLITE_FAILED of $SQLITE_CHECKED SQLite files failed integrity_check"
  for f in "${SQLITE_DBS[@]}"; do
    [ -s "$dir/staging/sqlite/$f" ] || { log "missing in snapshot: /staging/sqlite/$f"; missing=$((missing + 1)); }
  done
  [ "$missing" -eq 0 ] || die "$missing expected database copies are missing from the snapshot"

  STEP="check Postgres dumps"
  for spec in "${PG_DBS[@]}"; do
    read -r container db user <<<"$spec"
    f=$dir/staging/postgres/$db.sql
    [ -s "$f" ] || die "Postgres dump $db.sql missing or empty"
    tail -c 300 "$f" | grep -q 'PostgreSQL database dump complete' || die "Postgres dump $db.sql is incomplete"
    log "ok   staging/postgres/$db.sql ($(fmt_bytes "$(stat -c %s "$f")"))"
  done

  printf '%s\n' "$SQLITE_CHECKED" >"$WORK/tmp/restore-test.files"
  printf 'snapshot %s: %s SQLite files ok, %s Postgres dumps complete\n' \
    "${snap:0:8}" "$SQLITE_CHECKED" "${#PG_DBS[@]}" >"$WORK/tmp/restore-test.detail"
}

job_success() {
  local files detail
  files=$(cat "$WORK/tmp/restore-test.files")
  is_uint "$files" || files=0
  detail=$(cat "$WORK/tmp/restore-test.detail")
  status_update --argjson t "$JOB_FINISHED" --argjson n "$files" --arg d "$detail" \
    '.last_restore_test = {time: $t, ok: true, files_checked: $n, detail: $d}'
  notify 1 "Restore test passed" "Monthly restore test on $HOST_TAG: $detail (${JOB_DURATION}s)." "white_check_mark,floppy_disk"
}

job_failure() {
  status_update --argjson t "$JOB_FINISHED" --arg d "$JOB_ERROR" \
    '.last_restore_test = {time: $t, ok: false, files_checked: 0, detail: $d}'
  notify 5 "Restore test FAILED" "The monthly restore test on $HOST_TAG failed: the backups may not be restorable.

$JOB_ERROR" "rotating_light,floppy_disk"
}

run_job restore-test "$@"
