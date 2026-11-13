#!/bin/bash
# Turns Portal's request files into script runs.
#
# Portal (uid 1000) cannot run anything in this container. It can only
# drop a small JSON file into ~/platform/data/backup/requests/ (= /state/requests):
#     {"action":"backup"}
#     {"action":"restore-staging","snapshot":"latest"}      or 8-64 lowercase hex
# Every 5 seconds this loop looks at those files, oldest first, and treats
# them as UNTRUSTED input:
#   - symlinks, non-regular files, files of unexpected owners, files over
#     1 KiB: removed and logged, never read
#   - at most 1 KiB is read; it must be exactly one of the two shapes above
#     (no extra keys); anything else is removed and logged
#   - the file is deleted BEFORE the job runs (a request is used once)
#   - the job is a fixed script; the only value passed on is the snapshot
#     id, validated again by restore-staging.sh
# Jobs run one at a time (the scripts share a lock with the cron jobs).
. /scripts/lib.sh
set +e # a bad request must never kill the loop
JOB=watcher
HEARTBEAT=/tmp/watcher.heartbeat

# Only uid 1000 may delete in requests/ (no CAP_DAC_OVERRIDE here).
remove() { as_owner rm -f -- "$1" || log "could not remove $1"; }

handle() {
  local f=$1 base owner size mtime content verdict snap
  base=${f##*/}
  # The name is chosen by whoever wrote the file: only harmless characters
  # reach the log (no newlines or terminal escapes to fake log lines).
  base=${base//[^A-Za-z0-9._-]/?}

  if [ -L "$f" ]; then
    log "rejected $base: symlink (removed, not followed)"
    remove "$f"
    return
  fi
  if [ ! -f "$f" ]; then
    log "ignored $base: not a regular file"
    return
  fi
  owner=$(stat -c %u -- "$f") || return
  if [ "$owner" != "$OWNER_UID" ] && [ "$owner" != 0 ]; then
    log "rejected $base: owned by uid $owner (removed)"
    remove "$f"
    return
  fi
  # Give a writer that does not use rename() a moment to finish.
  # (stat prints digits; checked anyway before it reaches arithmetic)
  mtime=$(stat -c %Y -- "$f") || return
  is_uint "$mtime" || return
  if [ $(($(date +%s) - mtime)) -lt 2 ]; then
    return
  fi
  size=$(stat -c %s -- "$f") || return
  is_uint "$size" || return
  if [ "$size" -gt 1024 ]; then
    log "rejected $base: $size bytes (max 1024) (removed)"
    remove "$f"
    return
  fi
  content=$(timeout 2 head -c 1024 -- "$f")
  remove "$f"

  # Strict validation: exactly one JSON object with exactly these keys.
  # \A and \z anchor the WHOLE string (^ and $ would also match at a newline).
  verdict=$(printf '%s' "$content" | jq -r -s '
    if length != 1 or (.[0] | type) != "object" then "invalid"
    else .[0] |
      if keys == ["action"] and .action == "backup" then "backup"
      elif keys == ["action", "snapshot"] and .action == "restore-staging"
           and (.snapshot | type) == "string"
           and (.snapshot == "latest" or (.snapshot | test("\\A[0-9a-f]{8,64}\\z")))
        then "restore-staging \(.snapshot)"
      else "invalid" end
    end' 2>/dev/null)
  [ -n "$verdict" ] || verdict=invalid

  case "$verdict" in
    backup)
      if [ "${DID_BACKUP:-0}" = 1 ]; then
        log "request $base: backup (coalesced with the backup that just ran)"
        return
      fi
      log "request $base: backup"
      /scripts/backup.sh manual </dev/null
      log "request $base: backup.sh exited with $?"
      DID_BACKUP=1
      ;;
    "restore-staging "*)
      snap=${verdict#restore-staging }
      if [[ ! $snap =~ ^(latest|[0-9a-f]{8,64})$ ]]; then
        log "rejected $base: bad snapshot id"
        return
      fi
      log "request $base: restore-staging $snap"
      /scripts/restore-staging.sh "$snap" </dev/null
      log "request $base: restore-staging.sh exited with $?"
      ;;
    *)
      log "rejected $base: not a valid request (removed)"
      ;;
  esac
}

log "watching $REQUEST_DIR every 5 s"
while true; do
  touch "$HEARTBEAT"
  DID_BACKUP=0
  if [ -d "$REQUEST_DIR" ]; then
    # Oldest first: names are <unix nanoseconds>.json, so a sort by name is
    # a sort by time. Other names (e.g. Portal's temp files) are not ours.
    while IFS= read -r -d '' f; do
      handle "$f"
    done < <(find "$REQUEST_DIR" -mindepth 1 -maxdepth 1 -name '*.json' -print0 | sort -z)
  else
    log "WARNING: $REQUEST_DIR is missing"
  fi
  sleep 5
done
