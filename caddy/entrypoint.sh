#!/bin/sh
# Caddy's entrypoint: work out which addresses Caddy may listen on, start it,
# and keep watching those addresses (a tiny "supervisor").
#
# Caddy runs in the host's network namespace, so "bind" decides who can reach
# it. We listen on the Tailscale IP and on the current LAN IP only: never on
# 0.0.0.0 and never on the public IPv6 address.
#
# Why a supervisor: the LAN IP comes from DHCP and can change (new lease,
# other WiFi network, router reboot). Caddy binds the address it was given at
# start; if that address goes away, Caddy keeps listening on a dead address
# and the LAN can no longer reach anything. So every CHECK_INTERVAL seconds a
# watcher re-reads the LAN address. If it changed (or appeared, or
# disappeared) and stays changed for two checks in a row (a short WiFi
# blip does not restart anything), it stops Caddy gracefully and this script
# exits non-zero. Docker's "restart: unless-stopped" then starts the
# container again, which binds the new addresses. Nothing outside this
# container changes (no host sysctls, no 0.0.0.0).
#
# Processes:   sh (PID 1, this script)
#               |- caddy run            (waited for in the foreground)
#               `- watcher (subshell)   (sleeps, compares, may stop caddy)
#
# Busybox sh only (the image is caddy:2-alpine).
set -u

: "${TS_IP:?TS_IP must be set in .env}"
CHECK_INTERVAL="${CHECK_INTERVAL:-15}"
# Marker the watcher leaves when IT stopped Caddy (so we exit non-zero).
CHANGED_FLAG=/tmp/.bind-addrs-changed
rm -f "$CHANGED_FLAG"

log() { echo "entrypoint: $*" >&2; }

# Current IPv4 of the LAN interface (first one), or empty.
lan_ip() {
    [ -n "${LAN_IFACE:-}" ] || return 0
    ip -4 -o addr show dev "$LAN_IFACE" 2>/dev/null | awk '{print $4}' | cut -d/ -f1 | head -n1
}

# The full set Caddy should bind: Tailscale IP + LAN IP (if any).
bind_addrs() {
    lan=$(lan_ip)
    if [ -n "$lan" ] && [ "$lan" != "$TS_IP" ]; then
        echo "$TS_IP $lan"
    else
        echo "$TS_IP"
    fi
}

# Tailscale may come up after Docker at boot. Exit non-zero so that
# "restart: unless-stopped" retries until the address exists.
if ! ip -4 -o addr show | grep -q " ${TS_IP}/"; then
    log "${TS_IP} is not on any interface yet; retrying"
    sleep 3
    exit 1
fi

BIND_ADDRS=$(bind_addrs)
if [ "$BIND_ADDRS" = "$TS_IP" ] && [ -n "${LAN_IFACE:-}" ]; then
    log "no IPv4 on ${LAN_IFACE}; listening on Tailscale only"
fi
export BIND_ADDRS
log "listening on ${BIND_ADDRS}"

caddy run --config /etc/caddy/Caddyfile --adapter caddyfile &
CADDY_PID=$!

# ---------------------------------------------------------------------------
# Watcher. Only the LAN part can change here: the Tailscale IP is fixed, and
# if tailscaled restarts, the socket bound to it starts working again by
# itself when the address comes back.
# ---------------------------------------------------------------------------
watch_addrs() {
    pending=""
    while :; do
        sleep "$CHECK_INTERVAL"
        now=$(bind_addrs)
        if [ "$now" = "$BIND_ADDRS" ]; then
            [ -n "$pending" ] && log "bind addresses back to '${BIND_ADDRS}'; no restart"
            pending=""
            continue
        fi
        if [ "$now" != "$pending" ]; then
            # First sighting: confirm on the next check before acting.
            log "bind addresses look different: '${BIND_ADDRS}' -> '${now}'; re-checking in ${CHECK_INTERVAL}s"
            pending=$now
            continue
        fi
        log "bind addresses changed: '${BIND_ADDRS}' -> '${now}'; stopping Caddy so Docker restarts it on the new addresses"
        : > "$CHANGED_FLAG"
        kill -TERM "$CADDY_PID" 2>/dev/null
        # Caddy waits for open connections (dashboard live updates are
        # long-lived); do not wait forever.
        i=0
        while kill -0 "$CADDY_PID" 2>/dev/null && [ "$i" -lt 10 ]; do
            sleep 1
            i=$((i + 1))
        done
        kill -KILL "$CADDY_PID" 2>/dev/null
        exit 0
    done
}
watch_addrs &
WATCH_PID=$!

# ---------------------------------------------------------------------------
# Signals. `docker stop` sends TERM to PID 1 (this script). Pass it on to
# Caddy so it shuts down cleanly. A trapped signal interrupts `wait` below,
# which is why waiting happens in a loop.
# ---------------------------------------------------------------------------
STOPPING=0
on_signal() {
    STOPPING=1
    log "received stop signal; stopping Caddy"
    kill -TERM "$CADDY_PID" 2>/dev/null
}
trap on_signal TERM INT HUP

# Wait until Caddy has really exited and collect its exit status.
# `wait` returns early (status > 128) when a trapped signal arrives; Caddy
# is then still running (or not yet reaped), so wait again.
while :; do
    wait "$CADDY_PID"
    status=$?
    kill -0 "$CADDY_PID" 2>/dev/null || break
done
kill "$WATCH_PID" 2>/dev/null

if [ -e "$CHANGED_FLAG" ]; then
    log "exiting with status 1 so Docker restarts Caddy (caddy exit status ${status})"
    exit 1
fi
if [ "$STOPPING" = 1 ]; then
    log "stopped (caddy exit status ${status})"
    exit 0
fi
log "caddy exited by itself with status ${status}"
exit "$status"
