#!/bin/bash
# Quiet (priority 1: no sound) weekly summary to ntfy topic "backups"
# (crontab: Sundays 09:00 IST). Reads only status.json and the small history
# file written by backup.sh; it does not touch the repository, so it needs
# no lock and cannot interfere with a running job.
#
#   docker compose exec backup /scripts/weekly-summary.sh
. /scripts/lib.sh
JOB=weekly-summary

st=$(status_read)
now=$(date +%s)
week_ago=$((now - 7 * 86400))
added_week=$(awk -v t="$week_ago" '$1 >= t {s += $2} END {printf "%d", s}' "$WORK/history.txt" 2>/dev/null || echo 0)
runs_week=$(awk -v t="$week_ago" '$1 >= t {n++} END {printf "%d", n}' "$WORK/history.txt" 2>/dev/null || echo 0)

last_success=$(jq -r '.last_success // empty' <<<"$st")
last_status=$(jq -r '.last_run.status // "never run"' <<<"$st")
check=$(jq -r 'if .last_check == null then "never" else (if .last_check.ok then "ok" else "FAILED" end) end' <<<"$st")
check_t=$(jq -r '.last_check.time // empty' <<<"$st")
rt=$(jq -r 'if .last_restore_test == null then "never" else (if .last_restore_test.ok then "ok" else "FAILED" end) + " (" + (.last_restore_test.files_checked | tostring) + " SQLite files)" end' <<<"$st")
rt_t=$(jq -r '.last_restore_test.time // empty' <<<"$st")

msg="Target: $(jq -r '.target' <<<"$st")
Last successful backup: $(fmt_time "$last_success")
Last run: $last_status
Snapshots: $(jq -r '.snapshot_count' <<<"$st"), repository $(fmt_bytes "$(jq -r '.repo_size_bytes' <<<"$st")")
This week: $runs_week successful runs, $(fmt_bytes "$added_week") added
Last check: $check, $(fmt_time "$check_t")
Last restore test: $rt, $(fmt_time "$rt_t")"

# A failed or missing backup in the summary deserves a tag that stands out.
tags="floppy_disk"
if [ -z "$last_success" ] || [ $((now - last_success)) -gt 93600 ] || [ "$last_status" != ok ]; then
  tags="warning,floppy_disk"
fi
log "$msg"
notify 1 "Backups: weekly summary" "$msg" "$tags"
