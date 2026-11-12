# Decisions

Choices made where the brief was open or reality differed from it. Newest facts were checked on 12 Nov 2026.

## Things that differed from the brief

1. **The LAN IP is `192.168.1.7`, not `192.168.1.18`.** It comes from DHCP and has changed. Caddy therefore does not hard-code it: `caddy/entrypoint.sh` binds to whatever IPv4 the interface `LAN_IFACE` (`wlp0s20f3`) has at start. If the address changes, `docker compose restart caddy` picks up the new one. Tailscale access is unaffected. A DHCP reservation on the router would stop it changing.
2. **The bot UI is not running.** `groqbots-orchestrator-1` exited three weeks ago. I did not start it (not mine to change). `bot.exp.lab` is routed to its port `127.0.0.1:8080` and shows "Down" until you start it, which you can do from the dashboard's Containers panel.
3. **The disk is 98 GB, not 256 GB.** That is the size of the root logical volume. The dashboard shows what the filesystem reports.
4. **The Gitea repo was not created.** No Gitea credentials exist on the server, and creating an access token for your account through Gitea's admin CLI was blocked by the permission system. All work is committed locally in `~/platform`. See "Pushing this repo to Gitea" in the README.
5. **Ritual's UI was not reused.** You said mid-task to pick a design that suits this app instead. Portal has its own dark control-panel look: system fonts (San Francisco on iPhone, Segoe UI on Windows, so nothing to download), one mint accent, amber and red only for warnings.

## Domain and DNS

6. **`exp.lab` is kept.** I downloaded the Public Suffix List (16,623 lines) and searched it: there is no rule for `lab` or `exp.lab`. Under the list's default rule, `lab` is the suffix and `exp.lab` is a registrable domain, so browsers accept a cookie with `Domain=exp.lab` and share it with all subdomains. Note that `.lab` is not officially reserved for private use (`.internal` and `home.arpa` are). If `.lab` were ever sold publicly, your devices would still resolve `exp.lab` here, because Split DNS takes priority.
7. **CoreDNS answers only for `exp.lab` and only on the Tailscale IP.** Other names are refused, so it is not an open resolver. It always answers with the Tailscale IP. A LAN device without Tailscale would need a hosts-file entry pointing `exp.lab` names at the LAN IP.

## Networking

8. **Caddy, CoreDNS and Portal use host networking.** Reasons: the bot UI listens on `127.0.0.1:8080` only, which a bridged container cannot reach without changing the bot's compose file; Caddy sees real client IPs (needed for rate limiting); a failed bind at boot (Tailscale not up yet) makes the container exit and Docker retry, whereas a failed Docker port mapping leaves a container down; and `/proc/net/wireless` only shows the host's WiFi in the host network namespace. The cost is less isolation, offset by explicit binds: Caddy on two IPs, CoreDNS on one, Portal on loopback.
9. **Portal is the routing table.** Caddy has one wildcard site and asks Portal for the upstream of each hostname. That is what makes "one entry in apps.yaml" true. The cost: Gitea requests also make the (sub-millisecond, local) hop to Portal, and if Portal is down, proxied URLs fail. The old ports are the fallback.
10. **The dashboard is protected by Portal itself, not by `forward_auth`.** Same session check, one hop fewer.

## Auth

11. **Cookie lifetime is 400 days, session lifetime is 30 days sliding.** Forward-auth cannot refresh a cookie on responses that come from apps. So the browser keeps the cookie for the maximum browsers allow, and the server-side row is the authority: 30 days without use and it is gone.
12. **Argon2id at 19 MiB, 2 passes, 1 thread** (OWASP's minimum). Larger settings would not fit the 150 MB budget. One hash runs at a time.
13. **Rate limiting is per client IP and kept in memory.** Three free failures, then 2s doubling to 15 min. A Portal restart clears it. There is no global lockout, because any device on the tailnet could then lock you out.
14. **Session tokens are stored hashed** (SHA-256), so a database backup holds no usable cookies.
15. **State-changing requests must send `Origin: http://exp.lab`.** SameSite does not separate subdomains from each other.
16. **The Portal cookie is stripped before requests reach apps**, and `X-Portal-*` request headers from clients are dropped.
17. **TOTP setup prints a key and an `otpauth://` link, no QR code.** A QR renderer would be another dependency. Authenticator apps accept the key by hand. 2FA only turns on after a code verifies. Codes cannot be reused.
18. **Changing the password signs out all sessions.**
19. **`/metrics` needs a session through Caddy, and is open on `127.0.0.1:7080`** for a future Prometheus on this machine.

## Docker access

20. **`wollomatic/socket-proxy` instead of the more common Tecnativa one.** It filters by regular expression, so the allowlist is exactly three calls. Tecnativa's "restart" switch also allows stop and kill. It listens on a unix socket in `./run` and has `network_mode: none`.
21. **"Inspect" is not allowed**, although the brief permitted it. Inspect returns each container's environment variables, where your apps keep secrets (for example Focal's Ritual token), and the dashboard does not need it.
22. **The restart button on a stopped container starts it.** That is how Docker's restart call behaves, and it is useful (for example for the bot UI).

## Dashboard

23. **An app counts as "up" on any answer below HTTP 500.** A redirect to an app's own login page means the app is alive.
24. **Collection slows down when nobody is watching.** System stats every 2s with a dashboard open, every 30s otherwise. Containers every 10s, only with a dashboard open. Health checks every 30s always.
25. **Dark theme only.**
26. **Only the four apps you named are in `apps.yaml`.** Ledger (`:8095`), the DSA tracker (`:9100`) and Dalal are also running. Adding one would put a login in front of it, so that is left to you. The README shows the Ledger entry.

## Left untouched

27. SSH, Tailscale settings and firewall rules were not changed. No existing container was stopped, restarted or reconfigured.
28. `tailscale serve` forwards port 443 to `127.0.0.1:8480`, where nothing listens. It was like that before and matters only if you add HTTPS (README).
29. Gitea's `ROOT_URL` still says `http://192.168.1.18:3000/`. Clone URLs shown in Gitea's pages are therefore stale. Changing it now could confuse the old port, so it is listed as a later step in the README.

## Build

30. Go is not installed on the server. Portal is compiled and tested inside a `golang:1.26-alpine` container (`portal/Dockerfile`, `portal/gorun.sh`).
