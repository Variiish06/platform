#!/bin/bash
# Weekly repository check (crontab: Sundays 04:00 IST).
#
#   docker compose exec backup /scripts/check.sh
#
# "restic check" verifies the repository's structure: every snapshot points
# to trees and data that exist, the index matches the pack files. That does
# NOT read the data itself. --read-data-subset=N/10 additionally downloads
# and decrypts one tenth of the pack files and verifies their checksums. N
# rotates with the ISO week number, so every pack is read at least once
# every 10 weeks, while each run stays cheap (important once the repository
# lives on Google Drive, where reading everything weekly would be slow).
. /scripts/lib.sh

job_main() {
  local week n
  STEP="select repository"
  select_repo
  require_repo
  restic unlock >/dev/null 2>&1 || true

  week=$(date +%V)
  n=$((10#$week % 10 + 1))
  STEP="restic check (data subset $n/10)"
  log "restic check of $RESTIC_REPOSITORY, reading data subset $n/10"
  restic check --retry-lock 5m --read-data-subset="$n/10" 2>&1 | sed '/^[[:space:]]*$/d' >&2
}

job_success() {
  status_update --argjson t "$JOB_FINISHED" '.last_check = {time: $t, ok: true}'
}

job_failure() {
  status_update --argjson t "$JOB_FINISHED" '.last_check = {time: $t, ok: false}'
  notify 5 "Backup check FAILED" "restic check found a problem in the backup repository on $HOST_TAG.

$JOB_ERROR

Do not prune or delete anything; see RESTORE.md." "rotating_light,floppy_disk"
}

run_job check "$@"
