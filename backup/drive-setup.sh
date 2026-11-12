#!/usr/bin/env bash
# Connect the backups to Google Drive. Run ON THE SERVER, in ~/platform:
#
#     backup/drive-setup.sh
#
# Before that, on your Windows PC (full walk-through: RESTORE.md, "Google Drive"):
#     winget install Rclone.Rclone          (then open a NEW PowerShell window)
#     rclone authorize "drive" "eyJzY29wZSI6ImRyaXZlLmZpbGUifQ"
# sign in with Google in the browser that opens, allow access, and copy the
# token JSON that PowerShell prints between the "--->" and "<---End paste"
# lines: a single line starting with {"access_token": ...
#
# The odd-looking second argument is base64 for {"scope":"drive.file"}. It
# makes rclone ask Google for the NARROWEST permission that works:
# "drive.file" = rclone may only see and change files that rclone itself
# created. restic (through rclone) creates every file of its repository, so
# that is all it needs, and a leaked token cannot read the rest of your Drive.
#
# What this script does:
#   1. asks you to paste that token
#   2. writes secrets/rclone.conf (mode 600) with a remote called "gdrive"
#   3. tests it from inside the backup container: rclone lsd gdrive:
#   4. tells you the two commands that switch the backups over to Drive
set -euo pipefail
cd "$(dirname "$0")/.."
CONF=secrets/rclone.conf

command -v jq >/dev/null || { echo "jq is needed (sudo apt install jq)"; exit 1; }
docker compose ps --status running --services 2>/dev/null | grep -qx backup ||
  { echo "The backup container is not running: docker compose up -d backup docker-proxy-backup"; exit 1; }

echo "Paste the token JSON from 'rclone authorize' (one line, starts with {\"access_token\"), then press Enter:"
IFS= read -r token
# Must be a JSON object with the fields rclone wrote.
if ! jq -e 'type == "object" and has("access_token") and has("refresh_token") and has("expiry")' >/dev/null 2>&1 <<<"$token"; then
  echo "That does not look like the token JSON (needs access_token, refresh_token, expiry). Nothing changed."
  exit 1
fi
token=$(jq -c . <<<"$token") # normalise to one line

umask 077
if [ -s "$CONF" ]; then
  cp -p "$CONF" "$CONF.bak-$(date +%Y%m%d-%H%M%S)"
  echo "Previous config saved as $CONF.bak-*"
fi
# Rewrite the file IN PLACE (">" truncates the same file). The container
# mounts this single file; a new file (rm + create, or mv) would not be
# seen by the running container until it is recreated.
cat >"$CONF" <<EOF
# Google Drive remote for restic backups. Written by backup/drive-setup.sh.
# scope drive.file: rclone can only see files it created itself.
[gdrive]
type = drive
scope = drive.file
token = $token
EOF
chmod 600 "$CONF"
echo "Wrote $CONF"

echo "Testing from inside the backup container (rclone lsd gdrive:) ..."
if docker compose exec -T backup bash -c '. /scripts/lib.sh; BACKUP_TARGET=drive; prepare_rclone; rclone lsd gdrive:'; then
  echo
  echo "Google Drive works. (An empty listing is normal: rclone only sees its own files.)"
  echo "Now switch the backups to Drive:"
  echo "  sed -i 's/^BACKUP_TARGET=.*/BACKUP_TARGET=drive/' .env"
  echo "  docker compose up -d backup"
  echo "  docker compose exec backup /scripts/backup.sh manual     # first run creates the repository on Drive"
else
  echo
  echo "The test FAILED. Check the token (run 'rclone authorize' again) and retry this script."
  exit 1
fi
