#!/bin/sh
# Write the secret files the monitoring stack mounts, from values in .env.
# Safe to run any time (idempotent): it only rewrites a file whose content
# differs, and tells you whether Alertmanager needs a restart.
#
#   secrets/ntfy-alerts-token   NTFY_TOKEN_ALERTS (ntfy user svc-alerts,
#                               write-only on topic "system"), read by
#                               Alertmanager via credentials_file.
#
# Permissions: the directory is 700 and the file 600, both owned by you
# (uid 1000). Alertmanager runs as the same uid (compose.monitoring.yml
# user: PUID:PGID), so it can read the file; nobody else on the machine can.
# The value is never printed.
set -eu

cd "$(dirname "$0")/.."

if [ ! -f .env ]; then
    echo "monitoring-secrets: .env not found in $(pwd)" >&2
    exit 1
fi

# Take the last NTFY_TOKEN_ALERTS= line, without sourcing .env (sourcing
# would execute it as shell code). Strip optional surrounding quotes.
token=$(sed -n 's/^NTFY_TOKEN_ALERTS=//p' .env | tail -n 1 | sed -e 's/^"\(.*\)"$/\1/' -e "s/^'\(.*\)'\$/\1/")
if [ -z "$token" ]; then
    echo "monitoring-secrets: NTFY_TOKEN_ALERTS is empty in .env; run scripts/ntfy-provision.sh first" >&2
    exit 1
fi

umask 077
mkdir -p secrets
chmod 700 secrets

target=secrets/ntfy-alerts-token
tmp=$(mktemp secrets/.ntfy-alerts-token.XXXXXX)
# No trailing newline: the file content IS the token.
printf '%s' "$token" > "$tmp"
chmod 600 "$tmp"

if [ -f "$target" ] && cmp -s "$tmp" "$target"; then
    rm -f "$tmp"
    echo "monitoring-secrets: $target is up to date"
else
    mv -f "$tmp" "$target"
    echo "monitoring-secrets: wrote $target"
    echo "monitoring-secrets: if Alertmanager is running: docker compose restart alertmanager"
fi
