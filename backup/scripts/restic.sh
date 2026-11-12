#!/bin/bash
# Run any restic command against the CURRENT repository (BACKUP_TARGET),
# with the password and settings already in place. For humans:
#
#   docker compose exec backup /scripts/restic.sh snapshots
#   docker compose exec backup /scripts/restic.sh ls latest /staging
#   docker compose exec backup /scripts/restic.sh find '*.sql'
#   docker compose exec backup /scripts/restic.sh stats --mode raw-data
#
# Reading is always safe. Do NOT run "forget", "prune" or "key remove" by
# hand unless you know exactly why (RESTORE.md, "What not to do").
. /scripts/lib.sh
JOB=restic
select_repo
exec restic "$@"
