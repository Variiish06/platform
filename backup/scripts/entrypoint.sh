#!/bin/bash
# Container start: prepare directories and status, then run two processes
#   watcher.sh   - Portal's request files (every 5 s)
#   supercronic  - the schedule in /etc/crontab
# If either one dies, stop the other and exit: Docker's
# "restart: unless-stopped" then starts a fresh container. (tini, PID 1,
# forwards signals and reaps zombies.)
. /scripts/lib.sh
JOB=entrypoint

log "starting: $(restic version | head -n1), $(rclone version 2>/dev/null | head -n1), sqlite $(sqlite3 --version | cut -d' ' -f1), TZ=${TZ:-UTC}"

mkdir -p "$WORK/cache" "$WORK/tmp" "$WORK/logs" "$WORK/rclone" "$WORK/restore-test" "$STAGING"
chmod 700 "$WORK/cache" "$WORK/tmp" "$WORK/logs" "$WORK/rclone"
chmod 1777 "$WORK/restore-test"
# Left-overs of a restore test interrupted by a container stop.
for d in "$WORK"/restore-test/*; do
  [ -e "$d" ] || continue
  as_ids "$(stat -c %u -- "$d")" "$(stat -c %g -- "$d")" sh -c 'chmod -R u+rwX "$1"; rm -rf -- "$1"' sh "$d" || true
done

# requests/ must stay writable by uid 1000 (Portal creates the files).
if [ ! -d "$REQUEST_DIR" ]; then
  as_owner mkdir -m 755 "$REQUEST_DIR"
fi
[ "$(stat -c %u "$REQUEST_DIR")" = "$OWNER_UID" ] || log "WARNING: $REQUEST_DIR is not owned by uid $OWNER_UID; Portal cannot drop requests"

# A job that was running when the container stopped is not running now.
status_update '.running = ""'

# Configuration problems are reported now in the log, and loudly (ntfy) by
# the first job that runs; the container still starts so Portal can show it.
if ( select_repo >/dev/null 2>&1 ); then
  log "repository: ${BACKUP_TARGET}"
else
  log "WARNING: configuration problem: $( (select_repo 2>&1) | tail -n1)"
fi

set +e # from here on we handle exit codes ourselves
/scripts/watcher.sh &
WATCHER=$!
echo "$WATCHER" >/tmp/watcher.pid

# -passthrough-logs: job output goes to the log as is (not wrapped).
# -debug: also logs when each job will run next (in IST).
supercronic -passthrough-logs -debug /etc/crontab &
CRON=$!
echo "$CRON" >/tmp/supercronic.pid

stop() {
  log "stopping (signal)"
  kill -TERM "$WATCHER" "$CRON" 2>/dev/null
  wait
  exit 0
}
trap stop TERM INT

wait -n "$WATCHER" "$CRON"
log "ERROR: $( kill -0 "$WATCHER" 2>/dev/null && echo supercronic || echo watcher) exited; stopping the container so Docker restarts it"
kill -TERM "$WATCHER" "$CRON" 2>/dev/null
wait
exit 1
