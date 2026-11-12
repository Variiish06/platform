#!/usr/bin/env bash
# Provision ntfy users, access rules and tokens. Safe to run any number of
# times: existing values in .env are kept (nothing is rotated), only missing
# ones are generated.
#
#   cd ~/platform && scripts/ntfy-provision.sh
#
# How it works
#   ntfy is configured DECLARATIVELY: compose.notify.yml passes the lists
#   NTFY_AUTH_USERS / NTFY_AUTH_ACCESS / NTFY_AUTH_TOKENS (built from .env)
#   and ntfy makes its auth.db match them on every start. So this script
#   only has to (1) make sure .env holds every value, (2) (re)start the
#   container so ntfy picks them up, and (3) check that the result works.
#
# Values it manages in .env (section "# --- Phase A: ntfy ---"):
#   NTFY_ADMIN_USER            admin login for the phone app / web app
#   NTFY_ADMIN_PASSWORD        its password (32 random URL-safe characters)
#   NTFY_ADMIN_PASSWORD_HASH   bcrypt hash of it (what ntfy stores)
#   NTFY_SVC_PASSWORD_HASH     bcrypt hash of a random, discarded password,
#                              shared by the svc-* users (they use tokens)
#   NTFY_TOKEN_<NAME>          one token per service, tk_ + 29 [a-z0-9]
#
# To rotate one token: delete its line from .env and run this again.
# To change the admin password: delete NTFY_ADMIN_PASSWORD and
# NTFY_ADMIN_PASSWORD_HASH from .env and run this again.
set -euo pipefail

cd "$(dirname "$0")/.."
ENV_FILE=.env
IMAGE=$(sed -n 's/^ *image: *\(binwiederhier\/ntfy:[^ ]*\).*/\1/p' compose.notify.yml | head -1)
SECTION="# --- Phase A: ntfy ---"
TOKENS="PORTAL ALERTS BACKUP FOCAL RITUAL DALAL GENERAL"

[ -f "$ENV_FILE" ] || { echo "No .env here. Copy .env.example to .env first." >&2; exit 1; }
# .env holds secrets: owner read/write only.
chmod 600 "$ENV_FILE"

# random_string LENGTH CHARSET: read from the kernel's CSPRNG and keep only
# the allowed characters ("tr -dc" deletes everything else).
random_string() {
    local s=""
    while [ "${#s}" -lt "$1" ]; do
        s+=$(head -c 256 /dev/urandom | LC_ALL=C tr -dc "$2")
    done
    printf '%s' "${s:0:$1}"
}

# get VAR: the current value in .env (without surrounding single quotes).
get() {
    sed -n "s/^$1=//p" "$ENV_FILE" | tail -1 | sed "s/^'\(.*\)'\$/\1/"
}

# set_missing VAR VALUE [quote]: append VAR to our section unless it is
# already set to something. An empty line (VAR= or VAR='') is replaced.
# bcrypt hashes contain "$", which Docker Compose would try to expand, so
# they are written in single quotes (taken literally by Compose and shells).
added=0
set_missing() {
    if [ -n "$(get "$1")" ]; then return; fi
    sed -i "/^$1=\(''\)\?\$/d" "$ENV_FILE"   # drop an empty placeholder line
    grep -qxF "$SECTION" "$ENV_FILE" || printf '\n%s\n' "$SECTION" >>"$ENV_FILE"
    if [ "${3:-}" = quote ]; then
        printf "%s='%s'\n" "$1" "$2" >>"$ENV_FILE"
    else
        printf '%s=%s\n' "$1" "$2" >>"$ENV_FILE"
    fi
    echo "  generated $1"
    added=1
}

# bcrypt PASSWORD: hash with ntfy's own tool (same cost/format it expects).
bcrypt() {
    printf '%s\n%s\n' "$1" "$1" | docker run --rm -i --network none "$IMAGE" user hash 2>/dev/null | tail -1
}

echo "Checking .env ..."
set_missing NTFY_ADMIN_USER "$(id -un)"
set_missing NTFY_ADMIN_PASSWORD "$(random_string 32 'A-Za-z0-9_-')"
if [ -z "$(get NTFY_ADMIN_PASSWORD_HASH)" ]; then
    set_missing NTFY_ADMIN_PASSWORD_HASH "$(bcrypt "$(get NTFY_ADMIN_PASSWORD)")" quote
fi
if [ -z "$(get NTFY_SVC_PASSWORD_HASH)" ]; then
    # Nobody ever learns this password: it is generated, hashed, forgotten.
    set_missing NTFY_SVC_PASSWORD_HASH "$(bcrypt "$(random_string 40 'A-Za-z0-9')")" quote
fi
for t in $TOKENS; do
    # Same shape as "ntfy token generate": tk_ + 29 lowercase letters/digits.
    set_missing "NTFY_TOKEN_$t" "tk_$(random_string 29 'a-z0-9')"
done
[ "$added" = 1 ] || echo "  all values present, nothing generated"

# Sanity-check the values (a hand-edited .env could break ntfy's start-up).
for t in $TOKENS; do
    [[ "$(get "NTFY_TOKEN_$t")" =~ ^tk_[-_A-Za-z0-9]{29}$ ]] || { echo "NTFY_TOKEN_$t is not a valid ntfy token" >&2; exit 1; }
done
for h in NTFY_ADMIN_PASSWORD_HASH NTFY_SVC_PASSWORD_HASH; do
    [[ "$(get $h)" =~ ^\$2[aby]\$ ]] || { echo "$h is not a bcrypt hash" >&2; exit 1; }
done

mkdir -p data/ntfy
chmod 700 data/ntfy

echo "Starting ntfy (recreated only if its settings changed) ..."
docker compose up -d --wait ntfy

# check NAME EXPECTED CURL-ARGS...: print OK/FAIL for one HTTP status.
fail=0
check() {
    local name=$1 want=$2
    shift 2
    local got
    got=$(curl -s -o /dev/null -w '%{http_code}' "$@" || true)
    if [ "$got" = "$want" ]; then
        printf '  ok    %-44s %s\n' "$name" "$got"
    else
        printf '  FAIL  %-44s got %s, want %s\n' "$name" "$got" "$want"
        fail=1
    fi
}

echo "Verifying logins (each token must belong to its user) ..."
URL=http://127.0.0.1:2586
for t in $TOKENS; do
    check "token NTFY_TOKEN_$t is accepted" 200 -H "Authorization: Bearer $(get "NTFY_TOKEN_$t")" "$URL/v1/account"
done
check "admin user + password" 200 -u "$(get NTFY_ADMIN_USER):$(get NTFY_ADMIN_PASSWORD)" "$URL/v1/account"

if [ "$fail" = 1 ]; then
    echo "Something is wrong. See: docker compose logs ntfy" >&2
    exit 1
fi
echo "Done. Values are in .env (variables NTFY_*). See NOTIFY.md."
