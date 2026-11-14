# Platform: the front door of `experimentos`

One small stack that gives every app on this server a clean name, one login and one dashboard, and (since 12–13 Nov 2026) push notifications, metrics with alerts, and encrypted backups.

Contents: [URLs](#urls) · [Architecture](#architecture) · [Services](#services) · [What you must do once](#what-you-must-do-once) · [Morning steps](#morning-steps) · [iPhone ntfy app setup](#iphone-ntfy-app-setup) · [Notifications](#notifications) · [Monitoring and alerts](#monitoring-and-alerts) · [Backups](#backups) · [Caddy and a changing LAN IP](#caddy-and-a-changing-lan-ip) · [How to add a new app](#how-to-add-a-new-app) · [Operations cheatsheet](#operations-cheatsheet) · [Fresh-clone setup](#fresh-clone-setup) · [Adding HTTPS later](#adding-https-later) · [Troubleshooting](#troubleshooting)

Other documents: [NOTIFY.md](NOTIFY.md) (sending notifications), [RESTORE.md](RESTORE.md) (backups and restores), [LEARNING.md](LEARNING.md) (how it works, with line references), [DECISIONS.md](DECISIONS.md) (why it is built this way, and accepted risks).

## URLs

| URL | What | Portal login |
|---|---|---|
| `http://exp.lab/` | Dashboard: one-line status, apps (with 30-day uptime), system, memory breakdown, containers, backups, notifications | yes |
| `http://exp.lab/graphs` | Charts: CPU, RAM, memory by group, disk, temperature, network, battery, containers, backups (1 h to 30 days) | yes |
| `http://focal.exp.lab/` | Focal (pomodoro) | yes |
| `http://ritual.exp.lab/` | Ritual (habits, to-dos) | yes |
| `http://bot.exp.lab/` | Groq bots UI (stopped; shows "Down") | yes |
| `http://git.exp.lab/` | Gitea | no (Gitea's own login) |
| `http://ntfy.exp.lab/` | ntfy web app and API (notifications) | no: ntfy's own login, every topic denied to anonymous users |
| `http://vm.exp.lab/vmui/` | VictoriaMetrics query UI (ad-hoc PromQL) | yes |
| `http://vm.exp.lab/vmalert/` | vmalert UI: alert rules and their state | yes |
| `http://alerts.exp.lab/` | Alertmanager: active alerts, silences | yes |
| `http://exp.lab/api/notify` | JSON helper to send a notification ([NOTIFY.md](NOTIFY.md)) | no session; needs an ntfy token |
| `http://exp.lab/healthz` | Portal health | no |
| `http://exp.lab/metrics` | Portal's Prometheus metrics | yes (or direct on `127.0.0.1:7080`) |

Everything is reachable over Tailscale (and from the LAN). Nothing listens on the public internet. The ports in the services table below are loopback-only (`127.0.0.1`): only Caddy (port 80) and CoreDNS (port 53, Tailscale IP only) face the network.

## Architecture

```
 iPhone / Windows PC (Tailscale on)                         ┌──────────────┐
   │                                                        │   ntfy.sh    │ public relay: gets only a
   │ 1. "where is focal.exp.lab?"  Tailscale Split DNS      │ (upstream)   │ message ID + hashed topic
   ▼    sends *.exp.lab lookups to 100.118.33.89:53         └──────┬───────┘
 ┌──────────────┐                                                  │ wake-up via Apple Push (APNs)
 │   CoreDNS    │  answers every *.exp.lab name with 100.118.33.89 ▼
 └──────────────┘                                            iPhone ntfy app, which then fetches
   │                                                         the real message from ntfy.exp.lab
   │ 2. GET http://focal.exp.lab/   (inside the Tailscale tunnel)
   ▼
 ┌──────────────┐ 3. forward_auth: "may this pass, and where   ┌───────────────────────────────┐
 │    Caddy     │    does this hostname live?"                 │  Portal  127.0.0.1:7080       │
 │  :80 on the  │ ───────────────────────────────────────────▶ │  login, sessions (SQLite)     │
 │ Tailscale IP │ ◀─────────────────────────────────────────── │  dashboard, /graphs, /metrics │
 │  and LAN IP  │   200 + X-Portal-Upstream, or 302 to login   │  /api/notify, Backups panel   │
 └──────────────┘                                              │  reads data/apps.yaml         │
   │ 4. proxy to the upstream Portal named                     └───────────────────────────────┘
   ▼                                                              │ unix socket   │ reads ntfy (ro token),
  Focal 127.0.0.1:8787   Gitea  127.0.0.1:3000                    ▼ run/portal/   │ queries VictoriaMetrics,
  Ritual 127.0.0.1:8090  Bots   127.0.0.1:8080              ┌──────────────┐      │ drops backup request files
  ntfy  127.0.0.1:2586   VM 127.0.0.1:8428                  │ docker-proxy │      │
  Alertmanager 127.0.0.1:9093                               │ list, stats, │      │
                                                            │ restart only │      │
                                                            └──────┬───────┘      │
                                                                   ▼ /var/run/docker.sock (Docker)

 Notifications                                    Monitoring (all host network, loopback)
 ─────────────                                    ───────────────────────────────────────
 services, scripts ──POST + token──▶ ntfy         node_exporter :9101 ─┐  scraped every 30 s
 Portal /api/notify ──caller's token──▶ ntfy      Portal /metrics      ├──▶ VictoriaMetrics :8428 (30 days)
 Alertmanager ──webhook + template──▶ ntfy        vmalert, Alertmanager┘        ▲ queries every 30 s
 backup container ──failures──▶ ntfy                                            │
 ntfy ──▶ iPhone app, Portal panel                vmalert :8880 ──firing alerts──▶ Alertmanager :9093
                                                  Alertmanager ──▶ ntfy topic "system" ──▶ phone

 Backups
 ───────
 backup container (restic + sqlite3 + rclone + supercronic, no ports)
   ├─ reads app data dirs, copies SQLite DBs with ".backup"
   ├─ pg_dump in dalal/conveyor Postgres ──▶ docker-proxy-backup (exec only, own socket volume) ──▶ Docker
   ├─▶ local restic repo /var/backups/restic-local   (BACKUP_TARGET=local, same disk)
   └─▶ Google Drive via rclone                      (BACKUP_TARGET=drive, off-site; one target at a time)
   writes data/backup/status.json (Portal shows it) and run/textfile/backup.prom (node_exporter exposes it)
```

## Services

Eleven containers in the compose project `platform`. `docker-compose.yml` holds the first four and pulls in the rest with `include:`.

| Container | Image (version) | Listens on | Memory limit | Job |
|---|---|---|---|---|
| `caddy` | `caddy:2-alpine` (not pinned) | `:80` on Tailscale IP + LAN IP | 64 MB | Reverse proxy, the only thing facing the network |
| `coredns` | `coredns/coredns:1.12.1` | `:53` on Tailscale IP | 32 MB | DNS for `*.exp.lab` |
| `portal` | built from `./portal` (Go) | `127.0.0.1:7080` | 96 MB | Login, dashboard, forward-auth, graphs, `/api/notify`, memory breakdown (reads the host's `/proc` read-only) |
| `docker-proxy` | `wollomatic/socket-proxy:1.13.2` (digest pinned) | socket `run/portal/docker.sock` | 24 MB | Portal's filtered Docker access: list, stats, restart, stop |
| `ntfy` | `binwiederhier/ntfy:v2.37.2` | `127.0.0.1:2586` (+ `ntfy:80` on network `platform_internal`) | 64 MB | Notification hub (`compose.notify.yml`) |
| `victoriametrics` | `victoriametrics/victoria-metrics:v1.155.0` | `127.0.0.1:8428` | 160 MB | Time-series database and scraper, 30-day retention |
| `vmalert` | `victoriametrics/vmalert:v1.155.0` | `127.0.0.1:8880` | 48 MB | Evaluates alert rules every 30 s |
| `alertmanager` | `prom/alertmanager:v0.34.2` | `127.0.0.1:9093` | 48 MB | Groups, repeats, resolves and silences alerts; posts to ntfy |
| `node-exporter` | `prom/node-exporter:v1.12.1` | `127.0.0.1:9101` | 32 MB | Host metrics: CPU, RAM, disk, network, temperature, battery |
| `backup` | built from `./backup` (restic 0.18.1, rclone 1.75, supercronic 0.2.51) | nothing | 256 MB | Nightly encrypted backups, checks, restore tests |
| `docker-proxy-backup` | `wollomatic/socket-proxy:1.13.2` (digest pinned) | socket in volume `platform_backup-proxy-sock` | 24 MB | Lets `backup` run `pg_dump` in two Postgres containers, nothing else |

The monitoring files are in `compose.monitoring.yml`, the backup files in `compose.backup.yml`. The new containers (ntfy and everything below it) use about 160–210 MiB together, measured with `docker stats`. Their budget is 350 MiB. There is no Grafana, to stay inside that budget ([DECISIONS.md](DECISIONS.md) #31).

Files:

```
docker-compose.yml       Caddy, CoreDNS, Portal, docker-proxy; includes the three below
compose.notify.yml       ntfy (users, tokens and access rules are built from .env here)
compose.monitoring.yml   node-exporter, victoriametrics, vmalert, alertmanager
compose.backup.yml       backup, docker-proxy-backup
.env                     machine settings and secrets (gitignored; see .env.example)
caddy/Caddyfile          proxy rules (does not list apps)
caddy/entrypoint.sh      picks the addresses Caddy listens on and follows LAN IP changes
coredns/Corefile         the exp.lab zone
ntfy/server.yml          ntfy config; ntfy/templates/ formats Alertmanager's messages
monitoring/scrape.yml    what VictoriaMetrics collects
monitoring/alerts/*.yml  alert rules
monitoring/alertmanager.yml  who is told, how often
backup/                  backup image, crontab, scripts, excludes, drive-setup.sh
scripts/                 ntfy-provision.sh, monitoring-secrets.sh, monitoring-check.sh
data/apps.yaml           the list of apps  <- the file you edit
data/portal.db           SQLite: password hash, sessions (gitignored)
data/ntfy, data/vm, data/alertmanager, data/backup   service state (gitignored)
secrets/                 files made from .env: ntfy-alerts-token, rclone.conf (gitignored)
run/portal/              docker-proxy's socket for Portal; run/textfile/ backup metrics
portal/                  Go source of Portal
portalctl                runs Portal admin commands
tools/                   cs (Claude Code session manager) and its tests
```

Tools:

- **Claude sessions**: `cs` (tools/cs) lists, starts (`claude --continue` per project in tmux) and stops idle Claude Code sessions to free RAM; how-to: [SESSION-MANAGER.md](SESSION-MANAGER.md), reference: [SESSIONS.md](SESSIONS.md).
- Config: `~/.config/cs/projects.conf` (example: tools/cs.projects.example); tests: `tools/cs-test.sh`.

## What you must do once

### 1. Set your password

```bash
cd ~/platform
./portalctl set-password
```

It asks twice and does not echo. Minimum 10 characters. Only an argon2id hash is stored.

### 2. Add Split DNS in Tailscale

This tells your devices to ask this server for names ending in `exp.lab`.

1. Open <https://login.tailscale.com/admin/dns>.
2. Under **Nameservers**, click **Add nameserver** → **Custom…**.
3. In **Nameserver**, enter `100.118.33.89`.
4. Turn on **Restrict to domain** (this is what makes it "Split DNS").
5. In **Domain**, enter `exp.lab`.
6. Click **Save**.

On each device, Tailscale must be allowed to manage DNS (it is by default):

- iPhone: Tailscale app → profile icon → **Use Tailscale DNS settings** on.
- Windows: Tailscale tray icon → **Preferences** → **Use Tailscale DNS settings** ticked.

Check it from the PC:

```powershell
nslookup focal.exp.lab        # should answer 100.118.33.89
```

### 3. Open the dashboard

Type the address in full the first time, **including `http://` and the final `/`**:

```
http://exp.lab/
```

Without `http://`, browsers treat `exp.lab` as a search, and Safari may try HTTPS first. After the first visit, bookmark it or add it to the Home Screen (Safari → Share → Add to Home Screen).

What the dashboard shows, top to bottom:

- **Status line.** One sentence that says whether everything is fine, for example "All 7 apps up · last backup 2 h ago · no alerts firing". Anything that needs attention comes first, in amber or red, and is also spelled out in words ("Bots is down", "backup failed 01:20", "2 alerts firing: ContainerDown ×2"). Healthy things have no colour at all.
- **Apps.** One row per app: state (only shown when it is not up), response time, the last 30 days as thin daily segments (hover for the day's %), and uptime %. Apps that list their containers in `data/apps.yaml` (`containers:`) also get **Stop** / **Start** (see "Stopping and starting apps" below).
- **System.** CPU (with a short history line), load, RAM, disk, temperature, power, WiFi.
- **Memory.** What is using the RAM, split into groups that add up to the total: containers (by compose project), Claude Code sessions (each `claude` CLI with everything it started; idle ones can be stopped with `cs stop --idle`), the Docker engine, your other processes, system services, the kernel, page cache (memory Linux lends to file caching and takes back the moment a program needs it) and free memory. Click a group to see its rows. Portal reads the host's `/proc` read-only for this (see [DECISIONS.md](DECISIONS.md) #81); command lines are shown shortened, with anything that looks like a token or password replaced by `***`.
- **Containers.** A table: name, project, uptime, CPU, memory, and Restart + Stop (or Start). Click a column heading to sort; stopped containers are dimmed at the bottom.
- **Backups.** The latest run, success, repository, check and restore test, plus "Back up now" and "Restore to staging…".
- **Notifications.** The last messages from ntfy as a log; high/urgent ones have an amber/red rule.

The header has **Light/Dark**: the page follows your system setting until you press it, then remembers your choice in that browser.

## Morning steps

Three things need you, because they need your phone, your Google account or your password manager. The full list with the exact commands (and the passwords) is in `~/platform/MORNING.md` on the server. That file is gitignored and mode 600. Delete it when you are done.

1. Save `RESTIC_PASSWORD` from `.env` in your password manager. Without it no backup can ever be read.
2. Set up the ntfy app on the iPhone ([below](#iphone-ntfy-app-setup)).
3. Connect Google Drive and switch the backups to it ([RESTORE.md section 8](RESTORE.md#8-google-drive-off-site-copy)).

## iPhone ntfy app setup

1. Make sure **Tailscale is connected** on the iPhone. `ntfy.exp.lab` exists only inside the tailnet.
2. Install **ntfy** from the App Store (the publisher is Philipp Heckel).
3. In the app: **Settings** → **Default server** → enter `http://ntfy.exp.lab` exactly as written, with `http://` and no trailing slash. It must match `base-url` in `ntfy/server.yml`.
4. **Settings** → **Users** → **Add user**. Service URL `http://ntfy.exp.lab`. Username = the value of `NTFY_ADMIN_USER`, password = the value of `NTFY_ADMIN_PASSWORD`. Both are in `~/platform/.env` and in `MORNING.md`. To print them on the server: `grep -E '^NTFY_ADMIN_(USER|PASSWORD)=' ~/platform/.env`.
5. Tap **+** and subscribe to each topic, one at a time, on the default server: `system`, `backups`, `focal`, `ritual`, `dalal`, `general`. Allow notifications when iOS asks.
6. Send a test from the server:
   ```sh
   TOKEN=$(sed -n 's/^NTFY_TOKEN_GENERAL=//p' ~/platform/.env)
   curl -H "Authorization: Bearer $TOKEN" -H "Title: Hello" -d "Test from the server" http://127.0.0.1:2586/general
   ```
   It should appear on the phone within a few seconds, and in the dashboard's Notifications panel within 15 seconds.

**What instant delivery needs.** iOS does not let the app keep a connection open in the background. So for every message this server sends a small "poll request" to the public `ntfy.sh` (`upstream-base-url: https://ntfy.sh` in `ntfy/server.yml`). The request carries only the message ID and a SHA-256 hash of the topic URL (for example, of `http://ntfy.exp.lab/backups`). ntfy.sh relays it as a wake-up through Apple's push service (APNs). The phone wakes, then fetches the real message (title, text, priority) directly from `http://ntfy.exp.lab` over Tailscale, with your login. The title and text never leave the server. For this to work, the server needs outbound internet access to `ntfy.sh`, and the phone needs Tailscale connected when the wake-up arrives. Without the wake-up, messages still arrive, but only when iOS lets the app refresh in the background, which can take hours.

**Troubleshooting.**

- *Messages in the Portal panel but not on the phone*: Tailscale off on the phone, or notifications disabled for ntfy in iOS Settings. Open the app; it fetches what it missed (messages are kept 7 days).
- *"Unauthorized" / no messages*: wrong user or password in step 4. Check them with the web app at `http://ntfy.exp.lab` (Sign in).
- *The app refuses a plain `http://` server.* This was not tested tonight, because it needs the real phone. If it happens, the fix is HTTPS: follow [Adding HTTPS later](#adding-https-later), then use `https://ntfy.exp.lab` as the server and change `base-url` in `ntfy/server.yml` to match. Until then nothing is lost: every message also appears in the dashboard's Notifications panel.

## Notifications

[ntfy](https://docs.ntfy.sh/) is the hub. A service publishes a short message to a **topic** (`system`, `backups`, `focal`, `ritual`, `dalal`, `general`). The phone app and Portal's Notifications panel read from it. Every sender has a token that may publish to exactly one topic. Portal's token may only read. How to send from curl, Python or Go, which address to use from where, and how to add or rotate tokens: [NOTIFY.md](NOTIFY.md).

What sends what today:

| Topic | Sender | When |
|---|---|---|
| `system` | Alertmanager | an alert fires (priority 5 urgent or 3 normal) and when it resolves (3) |
| `backups` | backup container | any backup, check or restore failure (5); weekly summary on Sundays 09:00 IST (1, silent); monthly restore test passed (1) |
| `focal`, `ritual`, `dalal`, `general` | tokens are ready | nothing yet; wire them into the apps when you want |

A successful nightly backup sends nothing on purpose (see [Backups](#backups)).

## Monitoring and alerts

**What is collected.** VictoriaMetrics scrapes every 30 seconds (`monitoring/scrape.yml`):

- `node` (node_exporter, `127.0.0.1:9101`): CPU, load, RAM, filesystems, disk I/O, network, CPU temperature (`coretemp`), battery `BAT0` and charger `ADP1`, and the backup metrics from `run/textfile/backup.prom`.
- `portal` (`127.0.0.1:7080/metrics`): `portal_app_up{app}` and response times from the 30-second health checks, `portal_container_up{name}` (every container, 1 = running), container CPU % and memory, logins, `/api/notify` results.
- `victoriametrics`, `vmalert`, `alertmanager`: the monitoring stack watching itself.

Data is kept for 30 days in `data/vm` (not backed up; it is only history).

**Alert rules** live in `monitoring/alerts/` (`services.yml`, `host.yml`, `backup.yml`). vmalert evaluates them every 30 s. A rule must stay true for its `for:` time before it fires.

| Alert | Fires when | For | Severity (ntfy priority) |
|---|---|---|---|
| ContainerDown | a container is not running, and it was running at some point in the last 24 h | 2 min | urgent (5) |
| AppDown | an app in `apps.yaml` fails Portal's health check, and it was up in the last 24 h | 2 min | urgent (5) |
| TargetDown | VictoriaMetrics cannot scrape `portal` or `node` | 2 min | urgent (5) |
| OnBattery | charger `ADP1` offline or battery `BAT0` discharging | 1 min | urgent (5) |
| BatteryLow | battery under 30 % **while on battery** | 1 min | urgent (5) |
| BackupStale | no successful backup for 26 h, or the backup metric is missing altogether | 10 min | urgent (5) |
| HighMemory | more than 90 % of RAM in use (MemAvailable) | 5 min | normal (3) |
| DiskFull | root filesystem more than 85 % full (stays firing 15 min after it dips back) | 5 min | normal (3) |
| CpuHot | CPU package above 85 °C | 5 min | normal (3) |

Alertmanager (`monitoring/alertmanager.yml`) groups by alert name, waits 30 s for others to join a new group, sends at most one update per group every 5 min, repeats a still-firing alert every 4 h, and sends one "Resolved" message per group. Delivery: Alertmanager posts its webhook JSON straight to ntfy topic `system`. ntfy turns it into a readable message with `ntfy/templates/alert-urgent.yml` or `alert-normal.yml`. Portal is not involved, so alerts still arrive when Portal is down.

Because of the 24-hour rule, a container that stays down for more than a day stops alerting by itself (its alert resolves). The first alert already told you.

**Add or change a rule.**

1. Edit or add a file in `monitoring/alerts/` (copy an existing rule; set `labels.severity` to `urgent` or `normal`, and give it a `subject` and `summary` annotation).
2. Check it: `scripts/monitoring-check.sh`. It validates the rules, `alertmanager.yml` and `scrape.yml` in throwaway containers.
3. Load it: `docker compose restart vmalert`. Pending `for:` timers survive the restart (vmalert keeps its state in VictoriaMetrics). For Alertmanager changes: `curl -X POST http://127.0.0.1:9093/-/reload`.
4. Watch it at `http://vm.exp.lab/vmalert/` (rule state: inactive, pending, firing).

**Silence an alert** (planned maintenance, a known problem): open `http://alerts.exp.lab` → **New Silence** → add a matcher such as `alertname="DiskFull"` (or `name="focal"` for one container) → set the duration → enter a comment → **Create**. Silenced alerts still show in the UI but send nothing until the silence expires. You can also click the bell icon next to an active alert to pre-fill the matchers.

**Graphs.** `http://exp.lab/graphs` draws 14 charts from VictoriaMetrics for 1 h, 6 h, 24 h, 7 days or 30 days. The queries are fixed in Portal (`portal/internal/server/graphs.go`); the page never sends its own. For anything else, use `http://vm.exp.lab/vmui/` and type PromQL, for example `portal_container_memory_bytes` or `rate(node_network_receive_bytes_total{device="wlp0s20f3"}[5m])`. The Apps table on the dashboard shows 30 days of uptime (one thin segment per IST day). "Memory by group" is the dashboard's memory breakdown over time, as stacked areas.

## Backups

Full guide, including restores and disaster recovery: [RESTORE.md](RESTORE.md).

- **What**: Portal (`portal.db`, `apps.yaml`, ntfy state, Alertmanager state), Focal, Ritual, Ledger (database and `secret.key`), Gitea (database, repositories, config, keys), DSA tracker, the Ramayana IMAX bot's `state.db`, and the Postgres databases of Dalal and Conveyor (`pg_dump`, plain SQL). SQLite databases are copied with `sqlite3 .backup` and checked with `PRAGMA integrity_check` before restic reads them.
- **Not included**: app source code (only data), metrics history, logs, caches, `.env` and `secrets/`. Keep the restic password outside this machine.
- **When** (IST): backup daily 03:00; `restic check` (structure plus a rotating tenth of the data) Sundays 04:00; real restore test on the 1st of each month 04:30; quiet summary to ntfy Sundays 09:00.
- **Where**: one target at a time, set by `BACKUP_TARGET` in `.env`. `local` = `/var/backups/restic-local` on this same disk (the current setting; protects against mistakes, not against a dead disk). `drive` = Google Drive folder `experimentos-restic` via rclone (off-site; needs your sign-in once, see [Morning steps](#morning-steps)).
- **Retention**: 7 daily, 4 weekly, 6 monthly snapshots. Several runs on the same day keep only the newest, so "Back up now" replaces today's snapshot instead of adding one.
- **Encryption**: every snapshot is encrypted with `RESTIC_PASSWORD`. Lose it and every backup is unreadable.
- **Dashboard**: the Backups panel shows the target, last run, size, snapshots and the last check and restore test. **Back up now** runs a backup. **Restore to staging** unpacks the newest snapshot into `~/restore-staging/<time>-<id>/` (never over live data) and checks every database in it. Only the newest 3 staging restores are kept.
- **Alerts**: failures send an urgent ntfy message to `backups`. Success is silent. If backups silently stop, BackupStale fires after 26 h.

Running restic by hand (`backup/scripts/restic.sh` points restic at the current target with the password set):

```sh
cd ~/platform
docker compose exec backup /scripts/restic.sh snapshots              # list
docker compose exec backup /scripts/restic.sh ls latest /staging     # browse
docker compose exec backup /scripts/restic.sh stats --mode raw-data  # repository size
docker compose exec backup /scripts/backup.sh manual                 # back up now
docker compose exec backup /scripts/restore-staging.sh latest        # restore to ~/restore-staging
```

## Caddy and a changing LAN IP

The LAN IP comes from DHCP and can change. Caddy listens only on the Tailscale IP and the current LAN IP, never on `0.0.0.0`. `caddy/entrypoint.sh` is a small supervisor: every 15 seconds it re-reads the IPv4 address of `LAN_IFACE` (`wlp0s20f3`). If the address differs on two checks in a row, it stops Caddy and exits. Docker (`restart: unless-stopped`) starts it again on the new address. LAN access is back within about 30–45 seconds. Tailscale access is not affected, and a short WiFi drop does not restart anything. Check what happened with `docker compose logs caddy | grep entrypoint`.

Consequence: Caddy binds whatever IPv4 the WiFi has, including on a WiFi you do not trust. On such a network the login page, Gitea and ntfy are reachable from that LAN. Set `LAN_IFACE=` (empty) in `.env` and `docker compose up -d caddy` to listen on Tailscale only. A DHCP reservation on the home router keeps the address stable.

## How to add a new app

Add one entry to `data/apps.yaml`:

```yaml
  - id: ledger                 # becomes http://ledger.exp.lab/
    name: Ledger
    description: Expenses
    icon: wallet
    upstream: 127.0.0.1:8095   # where the app really listens
```

Save. Within 30 seconds the app is routed, behind the login, on the dashboard and health-checked. Nothing needs a restart, and no DNS or Caddy change is needed. It also gets an uptime bar and is covered by the AppDown alert.

- `auth: false` skips the Portal login (for apps with their own, like Gitea).
- `health: /some/path` changes the path the health check requests.
- The app must be reachable from the host. An app in Docker needs a published port, for example `127.0.0.1:8095:8095`.
- If the file has a mistake, the old version stays active and the dashboard shows the error.

- `containers: [name, ...]` gives the row a Stop/Start button for exactly those containers (names as in `docker ps -a`).

## Stopping and starting apps

**Stop** on an app row (or a container row) asks for confirmation, then stops the containers (`docker stop`, 10 s to shut down cleanly). They stay stopped, even across a reboot, until you press **Start** (or run `docker compose up -d` in that project). The app row then says "stopped" in plain ink instead of red "down".

- **Alerts.** Portal creates an Alertmanager silence for each stopped container (`name="<container>"`) and for the app (`app="<id>"`, AppDown): 30 days, created by `portal`. Your phone gets no ContainerDown/AppDown for something you stopped on purpose; the status line says "2 alerts silenced (stopped on purpose)". **Start** from Portal ends those silences about 3 minutes later (the alert needs a minute to resolve). If you start it some other way, the silence stays until it expires, or delete it at http://alerts.exp.lab. If Alertmanager is down, the stop still happens and the page warns you.
- **Protected.** Portal, Caddy, CoreDNS and the two socket proxies (compose project `platform`) have no Stop button and the API refuses with 403: stopping them would lock you out of the page you need to start them again. Restart still works.
- Every stop/start is logged with the container, the user and the client IP: `docker compose logs portal | grep -E "stop|start"`.

## How to change your password

```bash
cd ~/platform
./portalctl set-password
```

This also signs out every existing session. Other commands:

```bash
./portalctl sessions            # who is signed in
./portalctl sessions revoke     # sign out everywhere
./portalctl sessions create --ttl 15m --note test   # a short-lived session for scripts/tests (max 24h)
./portalctl sessions revoke <cookie value>          # end one session
./portalctl totp enable         # turn on 2FA (prints a key for your authenticator app)
./portalctl totp disable
./portalctl totp status
```

Forgot the password? Run `set-password` again. Anyone with shell access to the server can do this, which is the intended recovery path.

## Old direct ports (still open)

These still work exactly as before. Close them once you are happy with the new URLs.

| App | Old address | How to close it later |
|---|---|---|
| Focal | `http://100.118.33.89:8787` (all interfaces) | In `~/focal/docker-compose.yml`, make uvicorn listen on `127.0.0.1` instead of `0.0.0.0`, then `docker compose up -d` |
| Ritual | `http://100.118.33.89:8090` (all interfaces) | Set `RITUAL_HOST=127.0.0.1` in `~/apps/ritual/ritual.env` and restart Ritual |
| Gitea (web) | `http://100.118.33.89:3000` (all interfaces) | In `~/gitea/docker-compose.yml`, change `"3000:3000"` to `"127.0.0.1:3000:3000"`, then `docker compose up -d` |
| Gitea (SSH) | port `2222` | Keep it. Git over SSH does not go through Caddy |
| Bots | `127.0.0.1:8080` | Already local-only. Nothing to close |

When you close Gitea's port, also set `ROOT_URL = http://git.exp.lab/` in `~/gitea/data/gitea/conf/app.ini`, so the clone URLs Gitea shows are right. It currently says `http://192.168.1.18:3000/`.

## Pushing this repo

The repository has a remote called `github` (`https://github.com/Variiish06/platform.git`). Tonight's commits were **not** pushed; `git status` shows `ahead`. To push: `cd ~/platform && git push github main`. `.env`, `secrets/`, `MORNING.md` and all databases are gitignored and never pushed.

To also keep a copy in Gitea:

1. In Gitea, click **+** → **New Repository**, name it `platform`, leave "Initialize repository" unticked, and create it.
2. Then:

```bash
cd ~/platform
git remote add origin http://localhost:3000/kunchu47/platform.git
git push -u origin main      # asks for your Gitea username and password once
```

## Operations cheatsheet

All commands run in `~/platform`. Service names are the ones in the services table (`portal`, `ntfy`, `vmalert`, `backup`, ...).

```bash
docker compose ps                                  # status and health of all eleven
docker compose logs -f --tail 100 portal           # follow one service's log (Ctrl-C to stop)
docker compose restart vmalert                     # restart one service
docker compose up -d alertmanager                  # apply a changed compose file or .env to one service
docker compose up -d --build portal                # rebuild Portal after a code change (runs the tests)
docker stats --no-stream --format '{{.Name}}\t{{.MemUsage}}' | grep platform   # memory per container
portal/gorun.sh go test ./...                      # Go tests (Go runs in a container)

# Backups
docker compose exec backup /scripts/backup.sh manual
docker compose exec backup /scripts/restic.sh snapshots
docker compose exec backup cat /work/logs/backup.log

# A test session for curl, without typing the password (fixed lifetime, max 24h)
./portalctl sessions create --ttl 15m --note curl-test     # prints a cookie value
curl -s -o /dev/null -w '%{http_code}\n' -b 'portal_session=<value>' http://exp.lab/graphs
./portalctl sessions revoke <value>

# ntfy
docker compose exec ntfy ntfy token list           # all tokens (declared ones are marked)
scripts/ntfy-provision.sh                          # create missing users/tokens in .env, restart ntfy, test them
# rotate one token, e.g. Focal's:
sed -i '/^NTFY_TOKEN_FOCAL=/d' .env && scripts/ntfy-provision.sh
#   after rotating NTFY_TOKEN_ALERTS: scripts/monitoring-secrets.sh && docker compose restart alertmanager
#   after rotating NTFY_TOKEN_PORTAL: docker compose up -d portal
#   after rotating NTFY_TOKEN_BACKUP: docker compose up -d backup

# Monitoring
scripts/monitoring-secrets.sh                      # write secrets/ntfy-alerts-token from .env
scripts/monitoring-check.sh                        # validate rules, alertmanager.yml, scrape.yml
curl -s 'http://127.0.0.1:8428/api/v1/query?query=up' | jq '.data.result[] | {job: .metric.job, up: .value[1]}'
```

## Fresh-clone setup

On a new machine (restoring data as well? Follow [RESTORE.md section 7](RESTORE.md#7-disaster-recovery-a-new-machine)):

1. Docker and git installed, you in the `docker` group, Tailscale up. Then `git clone https://github.com/Variiish06/platform.git ~/platform && cd ~/platform`.
2. `cp .env.example .env && chmod 600 .env`. Fill in `TS_IP` (`tailscale ip -4`), `LAN_IFACE` (`ip -br addr`), `DOCKER_GID` (`getent group docker`), `PUID`/`PGID` (`id -u`, `id -g`), a fresh `VM_ADMIN_KEY`, and `RESTIC_PASSWORD` (the **old** one if you want to read existing backups, otherwise a new random one). Generate random values with `tr -dc 'A-Za-z0-9' </dev/urandom | head -c 48`. Leave the ntfy values empty.
3. Create the directories and files that must exist, owned by you, before the first start (otherwise Docker creates them as root, or creates a directory where a file is expected):
   ```sh
   mkdir -p run/portal run/textfile data/backup/requests ~/restore-staging secrets
   chmod 700 secrets
   install -m 600 /dev/null secrets/rclone.conf
   ```
4. `scripts/ntfy-provision.sh` (fills in the ntfy passwords and tokens, starts ntfy, tests every token), then `scripts/monitoring-secrets.sh`.
5. `docker compose up -d --build`, then `./portalctl set-password`.
6. Check: `docker compose ps` (all healthy), `http://exp.lab/`, and run one backup (`docker compose exec backup /scripts/backup.sh manual`). Until the first backup succeeds, the BackupStale alert fires on purpose.

## Adding HTTPS later

Plain HTTP is acceptable today because every byte travels inside Tailscale's encrypted tunnel. HTTPS becomes worth it if you want the browser padlock, Home Screen web apps with service workers or push, LAN access without Tailscale, or if the iPhone ntfy app refuses a plain-HTTP server.

Caddy can be its own certificate authority (CA):

1. In `caddy/Caddyfile`: delete `auto_https off`, change the three site addresses from `http://…` to `https://…`, and add `tls internal` inside the `{$DOMAIN}` and `*.{$DOMAIN}` blocks.
2. In `docker-compose.yml`, give Caddy a volume so its CA survives restarts: `- ./caddy/data:/data`.
3. Port 443 on the Tailscale IP is currently taken by `tailscale serve`. Check with `tailscale serve status` and turn it off (`tailscale serve --https=443 off`) or Caddy cannot bind.
4. `docker compose up -d caddy`, then copy the CA certificate out: `docker compose cp caddy:/data/caddy/pki/authorities/local/root.crt .`
5. Trust `root.crt` on each device:
   - iPhone: send yourself the file, install the profile, then Settings → General → About → Certificate Trust Settings → turn it on.
   - Windows: double-click → Install Certificate → Local Machine → Trusted Root Certification Authorities.
6. Set `COOKIE_SECURE=1` in `.env` and `docker compose up -d portal`. The cookie then gets the `Secure` flag and Portal builds `https://` links. You will need to sign in again.
7. For ntfy: set `base-url: "https://ntfy.exp.lab"` in `ntfy/server.yml`, `docker compose restart ntfy`, and change the server in the iPhone app to `https://ntfy.exp.lab` (re-subscribe to the topics). Update `-external.url` / `--web.external-url` in `compose.monitoring.yml` if you want `https://` links in alerts.

## Troubleshooting

| Symptom | Likely cause and fix |
|---|---|
| Browser searches for "exp.lab" or says site can't be reached | Type `http://exp.lab/` in full. Then check DNS: `nslookup exp.lab` must answer `100.118.33.89`. If it does not, redo the Split DNS steps and check "Use Tailscale DNS settings" on the device |
| DNS answers but the page does not load | Is Tailscale connected on the device? On the server: `docker compose ps` and `docker compose logs caddy` |
| Safari shows a certificate warning | It tried `https://`. Use `http://exp.lab/` |
| Login says "No password has been set yet" | Run `./portalctl set-password` |
| "Too many attempts" | Wait for the time shown. It doubles with each further failure (2s, 4s, 8s … 15 min) and resets after a correct login. Restarting Portal also clears it: `docker compose restart portal` |
| Signed in, but an app still sends you to the login page | The cookie did not reach the subdomain. Sign in at `http://exp.lab/` (not at an IP address), and check the browser is not blocking cookies |
| An app row shows "down · connection refused" | The app is not running. Find it under Containers and press its start/restart button |
| `502` from an app URL | Same as above: Caddy could not connect to the app's port |
| `404 No app is configured for …` | That hostname has no entry in `data/apps.yaml` |
| Dashboard says "Can't reach Docker" | `docker compose up -d docker-proxy` |
| New app does not appear | Wait 30 seconds. Then `docker compose logs portal` for an `apps.yaml` error |
| `git clone http://git.exp.lab/...` asks for a login | Expected: that is Gitea's own login |
| After a reboot nothing answers for a few seconds | Caddy and CoreDNS wait for Tailscale to come up and retry by themselves |
| LAN IP changed (DHCP) and LAN access stopped | Caddy follows the new address by itself within about 45 seconds ([above](#caddy-and-a-changing-lan-ip)). If not: `docker compose logs caddy`, then `docker compose restart caddy`. Tailscale access is not affected |
| Notifications panel says "ntfy not reachable" | `docker compose ps ntfy`; `docker compose logs ntfy` |
| Uptime bars say "History unavailable" or graphs are empty | VictoriaMetrics is down: `docker compose ps victoriametrics` |
| An alert keeps firing for something you know about | Silence it at `http://alerts.exp.lab` ([above](#monitoring-and-alerts)) |
| "Backup FAILED" on the phone | [RESTORE.md section 10](RESTORE.md#10-when-something-goes-wrong) |
| Locked out completely | The old direct ports still work, and SSH is untouched. `cd ~/platform && docker compose logs` |

To stop the whole stack: `docker compose down`. The apps keep running on their old ports, but alerts and backups stop too.
