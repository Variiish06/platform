#!/bin/bash
# Docker healthcheck: healthy when
#   - supercronic (the schedule) is alive,
#   - the watcher is alive and went round its loop in the last 30 s, unless a
#     job is running right now (then the watcher waits for it, by design),
#   - status.json is valid JSON.
# Whether the last BACKUP succeeded is NOT container health: that is the
# BackupStale alert / backup_last_run_status metric.
set -u
kill -0 "$(cat /tmp/supercronic.pid 2>/dev/null)" 2>/dev/null || { echo "supercronic not running"; exit 1; }
kill -0 "$(cat /tmp/watcher.pid 2>/dev/null)" 2>/dev/null || { echo "watcher not running"; exit 1; }
age=$(($(date +%s) - $(stat -c %Y /tmp/watcher.heartbeat 2>/dev/null || echo 0)))
if [ "$age" -gt 30 ] && flock -n /tmp/backup.lock true; then
  echo "watcher stuck (heartbeat ${age}s old, no job running)"
  exit 1
fi
jq -e 'type == "object"' /state/status.json >/dev/null 2>&1 || { echo "status.json unreadable"; exit 1; }
echo ok
