# Decisions

Choices made where the brief was open or reality differed from it. Newest facts were checked on 12 Nov 2026.

## Things that differed from the brief

1. **The LAN IP is `192.168.1.7`, not `192.168.1.18`.** It comes from DHCP and has changed. Caddy therefore does not hard-code it: `caddy/entrypoint.sh` binds to whatever IPv4 the interface `LAN_IFACE` (`wlp0s20f3`) has at start. If the address changes, `docker compose restart caddy` picks up the new one. Tailscale access is unaffected. A DHCP reservation on the router would stop it changing. (Update, phase 2: Caddy now follows a changed LAN IP by itself; see #36.)
2. **The bot UI is not running.** `groqbots-orchestrator-1` exited three weeks ago. I did not start it (not mine to change). `bot.exp.lab` is routed to its port `127.0.0.1:8080` and shows "Down" until you start it, which you can do from the dashboard's Containers panel.
3. **The disk is 98 GB, not 256 GB.** That is the size of the root logical volume. The dashboard shows what the filesystem reports.
4. **The Gitea repo was not created.** No Gitea credentials exist on the server, and creating an access token for your account through Gitea's admin CLI was blocked by the permission system. All work is committed locally in `~/platform`. See "Pushing this repo" in the README.
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
19. **`/metrics` needs a session through Caddy, and is open on `127.0.0.1:7080`** for a future Prometheus on this machine. (VictoriaMetrics now scrapes it there; see #33.)

## Docker access

20. **`wollomatic/socket-proxy` instead of the more common Tecnativa one.** It filters by regular expression, so the allowlist is exactly three calls. Tecnativa's "restart" switch also allows stop and kill. It listens on a unix socket in `./run` and has `network_mode: none`. (Since phase 2 the socket is in `./run/portal/`, and it is pinned by digest; see #65.)
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

---

# Phase 2 (12–13 Nov 2026): notifications, monitoring, backups

Plan: `PLAN-2.md`. What was built: ntfy (notifications), VictoriaMetrics + vmalert + Alertmanager + node_exporter (metrics and alerts), a restic backup container, and the matching Portal panels. Numbering continues from part 1.

## Footprint

31. **No Grafana.** Grafana alone idles at 100–150 MB. The new containers have a 350 MiB budget. They used about 155–180 MiB idle (VictoriaMetrics ≈ 90, backup 7–24, ntfy ≈ 20, Alertmanager ≈ 19, vmalert ≈ 12, node_exporter ≈ 9, docker-proxy-backup ≈ 3), up to ≈ 215 MiB during a backup, and ≈ 209 MiB on the morning of 13 Nov with VictoriaMetrics freshly restarted. Grafana would leave no headroom for restic as the data grows. Instead, Portal's `/graphs` page (uPlot charts from VictoriaMetrics) plus VictoriaMetrics' built-in `vmui` cover the need with zero extra containers. It also avoids a second login, or an auth proxy in front of Grafana that would be one more trust relationship to get wrong.
32. **Exact image versions, not `latest`.** Where the newest release was only days old, the previous one was taken so bug-fix reports had time to appear: VictoriaMetrics/vmalert v1.155.0 (v1.156.0 was 3 days old) and Alertmanager v0.34.2 (v0.35.0 was 3 days old). ntfy v2.37.2 and node_exporter v1.12.1 were the newest releases and already weeks old. The two socket proxies are pinned by digest as well (#65). Upgrades happen by changing a tag on purpose.

## Networking

33. **All monitoring services use host networking, bound to `127.0.0.1`.** VictoriaMetrics must scrape Portal, which listens on `127.0.0.1:7080` in the host namespace. node_exporter must see the host's `/proc`, `/sys` and network interfaces. Caddy and Portal must reach VictoriaMetrics and Alertmanager on loopback. A bridge network would need published ports plus `host.docker.internal` tricks for each of these. Every listener is explicitly `127.0.0.1`, and Alertmanager's cluster port is switched off (`--cluster.listen-address=`), so nothing is reachable from the LAN or tailnet.
34. **ntfy runs on a bridge network (`platform_internal`) with a loopback-published port.** ntfy needs no host access, so it gets the default isolation. Caddy and Portal reach it at `127.0.0.1:2586`, and the backup container reaches it by name at `http://ntfy:80` on the shared network.
35. **The monitoring UIs go through Caddy behind the Portal login**: `vm.exp.lab` (VictoriaMetrics, with vmalert's UI proxied at `/vmalert/` via `-vmalert.proxyURL`, so vmalert needs no hostname of its own) and `alerts.exp.lab` (Alertmanager). Neither has its own authentication, and the same session covers them with no second login.
36. **Caddy follows LAN IP changes by itself.** `caddy/entrypoint.sh` is a small supervisor. It starts Caddy, re-reads the LAN interface's IPv4 every 15 s, and acts only after two consecutive readings differ from what Caddy bound, so a WiFi blip does nothing. It then stops Caddy and exits non-zero, and Docker's `restart: unless-stopped` brings Caddy back on the new address. Rejected: binding `0.0.0.0` (would also listen on any other interface, including public IPv6), and the host sysctl `net.ipv4.ip_nonlocal_bind` (changes the host, needs sudo). SSH and Tailscale are untouched.

## Notifications

37. **`ntfy.exp.lab` is not behind the Portal login.** The iPhone ntfy app and services publishing with tokens cannot do a cookie login. ntfy enforces its own authentication instead: `auth-default-access: deny-all`, a fixed user list, no sign-up.
38. **iPhone push goes through `upstream-base-url: https://ntfy.sh`.** Only ntfy.sh holds the APNs key for the ntfy iOS app, and iOS does not allow a background connection. Per ntfy's documentation, only a poll request with the message ID and a SHA-256 hash of the topic URL goes to ntfy.sh. The phone fetches the content from `ntfy.exp.lab`. Rejected: no upstream (iPhone notifications could arrive hours late) and building our own iOS app.
39. **Alertmanager posts directly to ntfy, formatted by ntfy's server-side templates** (`ntfy/templates/alert-urgent.yml`, `alert-normal.yml`; selected with `?template=` in the webhook URL). Rejected: a custom bridge (for example a Portal webhook endpoint that reformats alerts). Delivery would then depend on Portal being up, and Portal-down is one of the things alerts must report.
40. **`POST /api/notify` forwards the caller's token.** Portal holds only a read-only ntfy token, for the Notifications panel. It has no write token, so a compromised Portal cannot publish, and ntfy alone decides which token may write which topic. Rejected: a Portal-wide publisher token, which would make Portal a privilege-escalation path (any caller could post anywhere).
41. **Strict input rules on `/api/notify`**: known topics only, JSON only, unknown fields rejected, body ≤ 8 KB, message ≤ 4096 bytes, title one line ≤ 250 characters, ≤ 10 tags of ≤ 32 safe characters, `click` must be an http(s) URL, 60 requests per minute per client. Errors are specific, so a service author sees what went wrong.
42. **ntfy users, access rules and tokens are declared, not created by hand.** `compose.notify.yml` builds `NTFY_AUTH_USERS/ACCESS/TOKENS` from `.env`, and ntfy makes its database match on every start. `scripts/ntfy-provision.sh` fills in missing values and never rotates existing ones. Each service user may write one topic and read none.

## Alerts

43. **Grouping and timing**: `group_by: [alertname]`, `group_wait 30s`, `group_interval 5m`, `repeat_interval 4h`, `send_resolved: true`. One message per kind of problem, at most one update per 5 minutes per group, and a reminder every 4 hours while it lasts. Severity is constant per alert name, so a group is never half urgent.
44. **ContainerDown and AppDown only fire for things that were up within the last 24 h.** Otherwise the long-stopped bot containers and the `bot` app would alert forever. Side effect, accepted: an alert for something that stays down resolves by itself after 24 h. The first alert already told you.
45. **`keep_firing_for: 15m` on DiskFull**, so a disk hovering around 85 % does not fire and resolve repeatedly.
46. **BatteryLow only fires while on battery** (capacity < 30 % and the charger offline). A battery recharging after a power cut must not keep the alert open. OnBattery (charger offline or battery discharging, 1 min) is urgent because this laptop is its own UPS.
47. **One `TargetDown` rule for `portal` and `node_exporter`** instead of PLAN-2's separate PortalDown and NodeExporterDown. Same effect, half the rules. It matters because without those targets most other rules go blind.
48. **BackupStale is a dead man's switch**: it fires when the last successful backup is more than 26 h old **or** the metric is absent. So it also fires on a fresh install until the first backup succeeds.

## Portal

49. **`/graphs` uses a fixed server-side allowlist of PromQL.** The browser sends only a chart name and one of five ranges, never a query. A signed-in page cannot become a general query console, and a bug in the page cannot run an expensive query. Answers are cached 20 s and identical concurrent requests share one query.
50. **Uptime % = `avg_over_time(portal_app_up[1d])`, evaluated at IST midnights** (one range query with a 1-day step; today separately up to now). The overall figure weights days by their sample count, so a partial first day does not count as a full one. Percentages are rounded **down**: 99.96 % shows as 99.9 %, and only a perfect record shows 100 %.
51. **`portal sessions create`** (via `./portalctl`) makes a fixed-lifetime session, at most 24 h, that does not slide with use. It was used to test protected URLs without typing the password. It needs shell access, like `set-password`. End one early with `sessions revoke <value>`.
52. **Container metrics are collected in the background every 30 s** (every 10 s while a dashboard is open), whether or not anyone is watching. Scrapes and page loads only read the cache, so a slow Docker never slows them. Stopped containers are exported as `portal_container_up 0` rather than left out, so "stopped" is a visible value change.

## Backups

53. **Postgres is dumped with `docker exec … pg_dump` through a dedicated exec-only socket proxy** (`docker-proxy-backup`). Rejected: joining the backup container to Dalal's and Conveyor's private networks and storing their database passwords here. That couples this project to two other projects' networks and secrets and breaks when they change. The cost is #66.
54. **Plain, uncompressed SQL dumps.** restic deduplicates unchanged parts between nights and compresses the repository itself. A compressed dump changes almost entirely when one row changes.
55. **Gitea is backed up as a SQLite `.backup` of `gitea.db` plus its file tree** (repositories, LFS, config, keys), not with `gitea dump`. `gitea dump` writes one big zip every night, which defeats deduplication, and it would need exec rights inside the Gitea container.
56. **Every SQLite database is copied with `sqlite3 .backup` as the user that owns it, then `PRAGMA integrity_check`.** A plain copy of a live (especially WAL) database can be torn or stale. Running as the owner means no root-owned `-wal`/`-shm` file can appear next to a live database. The raw database files are excluded from the file walk.
57. **Staging (database copies and dumps) lives on a Docker volume**, not in a host directory. The copies contain secrets, and root in the container needs a directory it owns without `CAP_DAC_OVERRIDE`.
58. **Restores run as `restic dump --archive tar | tar -x` with tar running as uid 1000.** Files come out owned by you, even root-owned originals such as Gitea's ssh host keys, and the container needs neither `CHOWN` nor `DAC_OVERRIDE`. Restores only ever go to `~/restore-staging/<time>-<id>/`; the script builds the path itself and keeps the newest 3.
59. **No ntfy message on a successful nightly backup.** A daily "OK" trains you to ignore the topic. Instead there is a quiet weekly summary (Sunday 09:00, priority 1) and the BackupStale dead man's switch. Failures are urgent.
60. **`restic check` reads a rotating 1/10 of the pack data weekly**, so all data is verified every 10 weeks. Reading everything weekly would be slow and costly on Google Drive.
61. **One backup target at a time** (`BACKUP_TARGET=local|drive`). Two repositories would double the run time and the memory peak within the 350 MiB budget. Switching is one line in `.env`, and the local repository keeps its history.
62. **rclone uses the `drive.file` scope**: it can see only files it created itself, not the rest of your Drive. restic creates every file of its repository, so that is all it needs, and a leaked token exposes nothing else.
63. **Not backed up, on purpose**: exec-engine's database (the service is stopped), groqbots (ignored by request), metrics history, caches, logs, and **app source code**. Backups are data only. The source of Ledger, Conveyor and the Ramayana bot exists only on this disk (see MORNING.md: push them somewhere).
64. **Several snapshots on the same day collapse under `--keep-daily`.** "Back up now" therefore replaces today's snapshot after pruning rather than adding a new one. Accepted: the newest state of the day is what matters.

## Security review

65. **A separate reviewer found nothing reachable by an unauthenticated device or by a cross-site web page.** The following were fixed afterwards (commit f525cbd):
    - **A1**: a value from `data/backup/status.json` reached bash arithmetic in the root backup container (`weekly-summary.sh`), where `a[$(cmd)]` runs a command. All status reads are now rebuilt from the schema (`JQ_SANITIZE`) and numbers are checked with `is_uint`/`status_num` in `backup/scripts/lib.sh`.
    - **B1**: Portal no longer sees `data/ntfy`, `data/vm` or `data/alertmanager` (masked with empty tmpfs mounts), and mounts only `./run/portal` for its Docker proxy socket. The socket moved from `run/docker.sock` to `run/portal/docker.sock`.
    - **B2**: the backup proxy's socket moved to the named Docker volume `platform_backup-proxy-sock` (root-owned, mode 600), out of reach of uid 1000.
    - **B3**: Portal's SSE hub normalises `\r`, so notification text cannot forge dashboard events.
    - **B4**: sqlite3 and tar children run with a clean environment, so they never see the restic password or the ntfy token.
    - **B5**: VictoriaMetrics' admin endpoints (delete, snapshot, force-merge, reload, flags, pprof, …) require `VM_ADMIN_KEY` from `.env`.
    - **B6**: both socket-proxy images pinned to `1.13.2@sha256:…`.

## Accepted risks

66. **A compromise of the backup container is about equal to root on the host.** The exec proxy filters method and path, not the request body, so exec-create can ask for any command, as root and privileged, in the two Postgres containers. Mitigations: no listening ports, only three capabilities (`DAC_READ_SEARCH`, `SETUID`, `SETGID`), strict validation of Portal's request files, sanitised inputs (#65 A1), clean child environments.
67. **The backup container has read-write mounts on directories with live SQLite databases, and `SETUID`/`SETGID`.** Read-write is needed because even a reader of a WAL database writes the `-shm` index. The capabilities are needed to run sqlite3 as each database's owner (#56).
68. **The nightly backup fails closed.** A file named `*.db` that is not SQLite, or a database path with unusual characters, fails the whole run loudly (urgent ntfy) instead of being skipped silently. Fix by adding the path to `SQLITE_IGNORE` in `backup/scripts/lib.sh`.
69. **`ntfy.exp.lab` serves ntfy's login page, `/v1/stats` and docs without the Portal login** (#37). No topic content is readable without a login or token.
70. **An ntfy service-token holder can mint more tokens for the same user** via `/v1/account/token`. They get the same rights (one topic, write-only), but a leaked token can outlive its rotation. After a suspected leak: `docker compose exec ntfy ntfy token list`, remove unknown tokens of that user, and rotate the declared one (NOTIFY.md).
71. **Loopback is trusted.** Any local process can read Portal's `/metrics` directly, query VictoriaMetrics, and send `X-Forwarded-For` to Portal on `127.0.0.1:7080` to dodge the per-IP rate limits. There is one user on this machine.
72. **Secrets are visible to anyone in the `docker` group** via `docker inspect` (environment variables). Being in the docker group is root-equivalent anyway.
73. **`~/restore-staging/*` holds full copies of databases and keys.** The directories are mode 700, only the newest 3 are kept, and you should delete them when done.
74. **On an untrusted WiFi, Caddy is reachable there too** (#36): it binds whatever IPv4 the WiFi interface has, so the login page, Gitea and ntfy's login page would be reachable from that network. Set `LAN_IFACE=` empty when travelling.
75. **`.env` and `secrets/` are deliberately not in the backups.** A backup that contains its own decryption key protects nothing. Consequence: `RESTIC_PASSWORD` must be stored in your password manager, or every backup is unreadable after a disk loss.
76. **`caddy:2-alpine` is still an unpinned tag** (part 1 decision, unchanged). A pull can change the Caddy version.
77. **Plain HTTP inside the tailnet** stays (part 1). Tailscale encrypts the traffic. Untested: whether the iPhone ntfy app accepts a plain-`http://` server. That needs the real phone. Fallback: README "Adding HTTPS later".

## Housekeeping

78. **`MORNING.md` is gitignored** (mode 600) because it contains the ntfy admin password and the restic password. Delete it after reading.
79. **Nothing was pushed.** The repository has unpushed commits on `main` (remote `github`). Pushing is your call.

## Portal redesign and memory breakdown (13 Nov 2026)

80. **Design direction: an instrument readout, not a SaaS dashboard.** Your verdict on the old look was "AI-generated slop", so Portal was redesigned. What went: floating rounded cards with shadows, the gradient glow, icon tiles, green "OK" dots everywhere, pill badges. What replaced it: one centred column (max 1100 px), sections separated by a hairline rule with a small uppercase label in the left margin, tables for apps and containers, a readings row for the system, corner radius 0–2 px. Fonts are the system sans for prose and the system monospace (tabular figures) for every number, name, path and time. The type scale has five sizes: 11, 13, 15, 18 and 22 px. **Quiet when healthy:** healthy things are plain ink; amber means warning, red means down/failed, and every state is also written in words. Palette (warm, desaturated): dark `#12110e` background, `#e9e4d6` ink, `#a8a396`/`#8a8579` secondary/tertiary; light ("paper") `#f4f1e8` / `#1c1a15`. All text pairs were checked for AA (lowest: tertiary ink, 5.1:1 dark, 5.2:1 light). **Accent: ink blue** (`#8db2e3` dark, `#2a5a9b` light), used only for links, buttons, focus and the "today" marker. It was chosen over burnt orange because orange sits next to the amber warning colour, and then "clickable" and "warning" would look alike. Blue never means a state here. The theme follows `prefers-color-scheme` until you press Light/Dark; the choice is applied by `theme.js` from `<head>` before the first paint (the CSP forbids inline scripts). CSP, server-rendered fragments over SSE and the vendored uPlot are unchanged; no new dependencies.
81. **Portal reads the host's `/proc` (read-only bind mount at `/host/proc`), not `pid: host`.** This is the least privilege that answers "what is taking the RAM". Portal stays uid 1000, `cap_drop: ALL`, read-only, with no-new-privileges and Docker's default AppArmor profile. **What it can now see:** every host process's pid, parent, name, uid, cgroup (so, which container it is in), memory counters and full command line. Command lines are world-readable on Linux anyway, but tools still pass secrets on them, so the page shows only the program's base name plus arguments with anything after `--token`/`--password`/`-p`/`-H`, any `NAME=value` whose name looks secret, `Authorization`/`Bearer` values, known token shapes (`sk-`, `ghp_`, `tk_`...) and URL passwords replaced by `***`, cut to 60 characters (`portal/internal/procmem/procmem.go`, `Sanitize`). **What it cannot see:** other processes' environment, open files, working directory, memory contents or `smaps`. Those need ptrace access, which AppArmor refuses for processes outside the container (tested). The tmpfs masks over `data/ntfy`, `data/vm` and `data/alertmanager` are unchanged and still hold. Remove `PORTAL_HOST_PROC` and the mount to switch the panel off.
82. **Claude Code's `~/.claude/sessions` is mounted read-only** (`/host/claude-sessions`) because `/proc/<pid>/cwd` is blocked (#81). The `<pid>.json` files give each session's working directory, name and idle/busy state. Portal opens only files named `<digits>.json` under 16 KB, and trusts one only when its recorded process start time matches the live process (a reused pid does not inherit a stale session). The directory also holds per-session `.key` files (mode 600, readable by uid 1000, hence by Portal). Portal never opens them, and they are useless to it: they authenticate on a socket under the host's `/tmp`, which Portal cannot reach. Accepted as low risk; remove the mount and `PORTAL_CLAUDE_SESSIONS` if you disagree (sessions then show their pid instead of a path).
83. **Memory per process is RssAnon + RssShmem, not VmRSS and not PSS.** VmRSS includes file pages that are already counted under page cache, and shared libraries counted once per process. PSS would be fairest but is unreadable (#81). Summed over the host this matches `/proc/meminfo`'s AnonPages + Shmem within ~1%. Containers are measured the same way (their processes' sum), so the Memory panel's container figures are lower than `docker stats` (which adds the container's active page cache). The Containers table keeps showing Docker's figure, labelled as such. Whatever does not add up is shown as "unaccounted" (about 1–2% here), not hidden. The scan takes ~12 ms; it runs every 10 s with a dashboard open, otherwise every 60 s. Portal's RAM stayed at ~11 MiB (cgroup).
84. **Do not use the `ritual-ui` skill or Ritual's design system for Portal.** Your instruction of 13 Nov 2026. Portal keeps the look described in #80. This replaces the older #5.
85. **The status line reads firing alerts from VictoriaMetrics** (`ALERTS{alertstate="firing"}`, which vmalert already writes there) every 30 s, rather than asking Alertmanager. One fewer dependency, and it fails soft: "alerts unknown" in amber.
86. **"Stop idle Claude sessions" is a hint, not a button.** The Memory panel shows `cs stop --idle` (tools/cs, built separately) as text. Stopping processes from a web page would need far more privilege than Portal has, and would undo #81.
87. **Unknown paths get a styled 404 page** for browsers (Accept: text/html). Scripts still get plain text, and a GET to a POST-only path still gets 405.

## Stop/Start from the dashboard (14 Nov 2026)

88. **Portal can stop containers, not only restart them.** Your request: "stop Groq Bots (and apps generally) from the platform". The socket proxy's POST allowlist became `/v1\.[0-9]+/containers/[a-zA-Z0-9_.-]+/(restart|stop)` (image and digest unchanged). Still refused with 403/405 (tested live against the proxy socket): kill, start, exec, create, delete, `stop/../kill`, `stopx`, inspect. **No `start` endpoint:** Docker's restart of a stopped container starts it, so `start` would add privilege for nothing. Worst case if Portal were compromised is now "stop everything" instead of "restart everything"; both are an outage, neither gives code execution or secrets.
89. **Lifeline containers cannot be stopped from Portal** (HTTP 403, no Stop button): compose project `platform`, services `portal`, `caddy`, `coredns`, `docker-proxy`, `docker-proxy-backup`. Matched by compose labels, so `other-portal-1` of another project is not protected and a renamed platform container still is. Restart stays allowed, as before. The check is in Portal, not the proxy: the proxy filters paths by container *id*, which changes on every recreate. Other platform services (ntfy, VictoriaMetrics, vmalert, Alertmanager, backup, node-exporter) can be stopped; stopping them does not lock you out.
90. **App rows get Stop/Start only via an explicit `containers:` list in `apps.yaml`.** No guessing from compose projects: groqbots has a one-shot `bot-image` container that must never be started and a `testsite` test helper. The list is validated twice: names must look like Docker names when the file loads (a bad one keeps the previous file, as for any other mistake), and at use time they must match the live container list exactly; an unknown or protected name shows an amber error on the row, hides the button, and the API answers 409 without calling Docker. Set for **bot** (`groqbots-orchestrator-1`), **focal** (`focal`) and **git** (`gitea-gitea-1`, the only container of the gitea project). Not for **Ritual** (a host process, no container), ntfy/VictoriaMetrics/Alertmanager (platform services, use the container table if you must).
91. **Confirm with the existing dialog, not a second click.** Restart already confirms with a modal `<dialog>` (CSP-safe, keyboard and screen-reader friendly). Stop uses the same dialog, with a red confirm button, which is a stronger confirmation than "click again within 3 s" and keeps one interaction pattern on the page.
92. **Stopping silences alerts in Alertmanager; starting ends the silence 3 minutes later.** One silence per matcher: `name="<container>"` (ContainerDown and any future per-container alert) and `app="<id>"` (AppDown) for apps that list the container (also when stopped from the container table). 30 days, `createdBy: portal`, comment "stopped from Portal by <user>". On Start (container Start/Restart or app Start) Portal looks up its active silences for exactly that matcher (so ones from before a Portal restart are found, and other people's silences are left alone) and **moves their end to now + 3 min instead of deleting them**: right after a start vmalert still reports the container as down for up to a minute, and Alertmanager flushes the ContainerDown group every 5 min (`group_interval`), so an immediate DELETE could send a stale ContainerDown to the phone. The end time is stored in Alertmanager, so it survives a Portal restart. If the update is refused, Portal falls back to DELETE. A second Stop reuses the live silence instead of adding another. If you start a container some other way (`docker compose up`), its silence stays until it expires or you delete it in Alertmanager's UI.
93. **Alertmanager being down never blocks a stop or start.** Portal logs it and the page shows a warning ("expect a ContainerDown notification"). The silence calls have a 5 s timeout and stop after the first failure. URL from `PORTAL_ALERTMANAGER_URL` (default `http://127.0.0.1:9093`, loopback, no auth; `off` disables). This adds Alertmanager as a soft dependency, but only for stop/start; the status line still reads alerts from VictoriaMetrics (#85).
94. **The status line no longer shouts about things stopped on purpose.** Alerts whose subject Portal holds a silence for are counted as "N alerts silenced (stopped on purpose)" in plain ink, and an app whose listed containers are all stopped reads "Bots stopped" and "all 6 running apps up" instead of red "Bots is down". Portal re-reads its silences from Alertmanager at start and every 5 minutes, so a silence expired by hand is forgotten here too.
95. **Every stop/start is logged** with container (or app and its containers), user and client IP, e.g. `stop app bot (groqbots-orchestrator-1) requested by kunchu01 from 100.118.33.89`; restart log lines gained the user too.
96. **Observed: `groqbots-orchestrator-1` ignores SIGTERM**: `docker stop` waits the full 10 s and then kills it (exit 137). Harmless for a stop, but the bots' orchestrator gets no clean shutdown; worth fixing in groq-bots (handle SIGTERM in the FastAPI process, or `init: true` / `STOPSIGNAL`).
