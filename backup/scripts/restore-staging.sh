#!/bin/bash
# Restore a snapshot to ~/restore-staging/ so you can look at it, copy single
# files out, or use it for a disaster recovery (RESTORE.md).
#
#   docker compose exec backup /scripts/restore-staging.sh latest
#   docker compose exec backup /scripts/restore-staging.sh 1a2b3c4d
#   ("Restore to staging" in Portal does the same via a request file.)
#
# Safety rules, enforced here:
#   - the argument is "latest" or 8-64 lowercase hex characters, nothing else
#   - the target directory is BUILT here, never taken from the input:
#       /restore-staging/<UTC yyyymmdd-HHMMSS>-<first 8 hex of the snapshot id>
#     and /restore-staging must be the bind mount of the host's
#     ~/restore-staging (nothing is ever restored over live data)
#   - files are unpacked by uid 1000, so you own them on the host
#   - only the newest 3 staging restores are kept (older directories that
#     match the pattern above are deleted; nothing else is touched)
. /scripts/lib.sh

KEEP=3
NAME_RE='^[0-9]{8}-[0-9]{6}-[0-9a-f]{8}$'

ARG=${1:-}
[[ $ARG =~ ^(latest|[0-9a-f]{8,64})$ ]] || die "usage: restore-staging.sh latest|<snapshot id, 8-64 hex>"

job_main() {
  local id name target n d
  rm -f "$WORK/tmp/restore-staging.path" "$WORK/tmp/restore-staging.detail"
  STEP="check target"
  mountpoint -q "$RESTORE_STAGING" || die "$RESTORE_STAGING is not a mount (expected host ~/restore-staging); refusing"

  STEP="select repository"
  select_repo
  require_repo

  STEP="find snapshot"
  if [ "$ARG" = latest ]; then
    id=$(restic snapshots --json --no-lock --host "$HOST_TAG" --latest 1 | jq -r 'sort_by(.time) | last | .id // empty')
  else
    # restic accepts a unique id prefix; it fails if none or several match.
    id=$(restic snapshots --json --no-lock "$ARG" | jq -r 'if length == 1 then .[0].id else empty end')
  fi
  [[ $id =~ ^[0-9a-f]{64}$ ]] || die "snapshot '$ARG' not found (or not unique)"

  STEP="restore"
  name=$(date -u +%Y%m%d-%H%M%S)-${id:0:8}
  target=$RESTORE_STAGING/$name
  # mode 700: the restore contains secrets (keys, password hashes, tokens).
  as_owner mkdir -m 700 -- "$target"
  echo "$target" >"$WORK/tmp/restore-staging.path"
  log "restoring snapshot ${id:0:8} into ~/restore-staging/$name"
  # restic streams the snapshot as a tar archive; tar running as uid 1000
  # unpacks it, so every file (even root-owned ssh host keys) ends up owned
  # by you, readable without sudo. Original modes are kept (minus umask 022).
  restic dump --no-lock --archive tar "$id" / | as_owner tar -x -f - -C "$target"

  STEP="check SQLite files"
  # (not "| tee": a pipeline would run it in a subshell and lose the counts)
  check_sqlite_tree "$target" >"$WORK/tmp/restore-staging.sqlite"
  cat "$WORK/tmp/restore-staging.sqlite" >&2
  n=$SQLITE_CHECKED
  printf 'snapshot %s: %s files, %s SQLite files checked, %s failed\n' "${id:0:8}" \
    "$(find "$target" -type f | wc -l)" "$n" "$SQLITE_FAILED" >"$WORK/tmp/restore-staging.detail"
  [ "$n" -gt 0 ] || die "no SQLite files in the restored snapshot"
  [ "$SQLITE_FAILED" -eq 0 ] || die "$SQLITE_FAILED of $n SQLite files failed integrity_check (files kept in ~/restore-staging/$name)"

  STEP="keep newest $KEEP"
  # Names sort by time. Only directories with OUR naming pattern, never
  # symlinks, never anything else a human put there.
  local ours=() i
  for d in "$RESTORE_STAGING"/*; do # a glob is sorted, oldest name first
    d=${d##*/}
    [[ $d =~ $NAME_RE ]] && [ -d "$RESTORE_STAGING/$d" ] && [ ! -L "$RESTORE_STAGING/$d" ] && ours+=("$d")
  done
  for ((i = 0; i < ${#ours[@]} - KEEP; i++)); do
    d=${ours[$i]}
    log "removing old staging restore ~/restore-staging/$d"
    as_owner chmod -R u+rwX -- "$RESTORE_STAGING/$d"
    as_owner rm -rf -- "${RESTORE_STAGING:?}/$d"
  done
}

record() { # ok(true|false) detail
  local path host_path
  path=$(cat "$WORK/tmp/restore-staging.path" 2>/dev/null || true)
  # Portal shows the HOST path, e.g. /home/kunchu01/restore-staging/2026...
  host_path=${path:+${HOST_RESTORE_STAGING:-$RESTORE_STAGING}/${path#"$RESTORE_STAGING"/}}
  status_update --argjson t "$JOB_FINISHED" --argjson ok "$1" --arg s "$ARG" --arg p "$host_path" --arg d "$2" \
    '.last_staging_restore = {time: $t, ok: $ok, snapshot: $s, path: $p, detail: $d}'
}

job_success() {
  record true "$(cat "$WORK/tmp/restore-staging.detail")"
}

job_failure() {
  record false "$JOB_ERROR"
  notify 5 "Restore to staging FAILED" "Restoring snapshot '$ARG' to ~/restore-staging failed on $HOST_TAG.

$JOB_ERROR" "rotating_light,floppy_disk"
}

run_job restore-staging "$@"
