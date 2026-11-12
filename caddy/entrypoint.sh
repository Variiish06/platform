#!/bin/sh
# Work out which addresses Caddy may listen on, then start it.
#
# Caddy runs in the host's network namespace, so "bind" decides who can reach
# it. We listen on the Tailscale IP and on the current LAN IP only: never on
# 0.0.0.0 and never on the public IPv6 address.
set -eu

: "${TS_IP:?TS_IP must be set in .env}"

# Tailscale may come up after Docker at boot. Exit non-zero so that
# "restart: unless-stopped" retries until the address exists.
if ! ip -4 -o addr show | grep -q " ${TS_IP}/"; then
    echo "entrypoint: ${TS_IP} is not on any interface yet; retrying" >&2
    sleep 3
    exit 1
fi

BIND_ADDRS="$TS_IP"
if [ -n "${LAN_IFACE:-}" ]; then
    LAN_IP=$(ip -4 -o addr show dev "$LAN_IFACE" 2>/dev/null | awk '{print $4}' | cut -d/ -f1 | head -n1)
    if [ -n "$LAN_IP" ]; then
        BIND_ADDRS="$BIND_ADDRS $LAN_IP"
    else
        echo "entrypoint: no IPv4 on ${LAN_IFACE}; listening on Tailscale only" >&2
    fi
fi
export BIND_ADDRS
echo "entrypoint: listening on ${BIND_ADDRS}"
exec caddy run --config /etc/caddy/Caddyfile --adapter caddyfile
