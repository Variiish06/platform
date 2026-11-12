# PLAN-2: notifications, monitoring, backups

Written by the orchestrator on 2026-11-12. Subagents: read this whole file before touching anything.
It is the contract between parallel workstreams. If reality forces a deviation, keep the contract
names stable where you can and say exactly what changed in your final report.

## Ground rules (apply to every workstream)

- Never touch SSH, Tailscale, firewall, or other projects' compose files/containers
  (Focal, Ritual, Gitea, Ledger, Dalal, Conveyor...). Never stop/restart those apps. No sudo (it is not passwordless).
- Everything lives in `~/platform/`, compose project `platform`. New services go in their own
  compose file pulled in by `include:` at the top of `docker-compose.yml`:
  `compose.notify.yml` (Phase A), `compose.monitoring.yml` (Phase B), `compose.backup.yml` (Phase C).
- Every new container: `restart: unless-stopped`, json-file logs `max-size 10m`, `max-file 3`,
  a `mem_limit`, pinned image tag (not `latest`), `cap_drop: [ALL]` plus only the caps it needs,
  `read_only: true` where practical, a healthcheck where the image makes one possible.
- Nothing listens on `0.0.0.0`. Host-network services bind `127.0.0.1` only. Bridge services publish
  ports as `127.0.0.1:PORT:...` only. Only Caddy faces the tailnet/LAN.
- Secrets only in `.env` (gitignored, chmod 600) or files under `secrets/` (gitignored). Add every new
  variable to `.env.example` with a comment and a placeholder, never a real value.
  Only append to `.env` / `.env.example` inside your own clearly marked section; other agents edit them concurrently.
- Go is not on the host: use `portal/gorun.sh go test ./...` / `go vet`; rebuild with `docker compose build portal`.
- Commit only your own paths (`git add <paths>`; never `git add -A`), retry if the index is locked. Never push.
- Host clock is UTC; user is in IST (`TZ=Asia/Kolkata`). Taken ports: 22, 53, 80, 631, 2222, 3000, 7080,
  8000, 8080, 8090, 8095, 8480, 8787, 9100.
- This is also a learning project: comment configs generously (why, not just what).

## Port and name map (all loopback)

| Service | Network | Listens | Container mem_limit |
|---|---|---|---|
| ntfy | bridge `platform_internal` + published | `127.0.0.1:2586` -> 80 | 64m |
| victoriametrics | host | `127.0.0.1:8428` | 160m |
| vmalert | host | `127.0.0.1:8880` | 48m |
| alertmanager | host | `127.0.0.1:9093` (cluster disabled) | 48m |
| node-exporter | host | `127.0.0.1:9101` | 32m |
| backup | bridge `platform_internal` | none | 256m |
| docker-proxy-backup | none (unix socket in `./run/backup-proxy/`) | - | 24m |

Actual combined usage of new containers must stay < 350 MB (`docker stats`).
**No Grafana**: it idles at 100-150 MB, which leaves no headroom for restic runs inside 350 MB.
Graphs are a Portal page (`/graphs`) drawing uPlot charts from VictoriaMetrics.

## Phase A: ntfy

- `ntfy.exp.lab` via `data/apps.yaml` entry `id: ntfy`, `auth: false`, upstream `127.0.0.1:2586`
  (no Portal login: the iPhone app cannot do cookie logins; ntfy enforces its own auth).
- ntfy config: `base-url: http://ntfy.exp.lab`, `behind-proxy: true`, `auth-default-access: deny-all`,
  `upstream-base-url: https://ntfy.sh`, signup/web-push off, message cache on disk (>= 7 days), state in `./data/ntfy/`.
- Topics: `system`, `backups`, `focal`, `ritual`, `dalal`, `general`. Priorities: 5 urgent, 3 normal, 1 quiet.
- Users and tokens (idempotent provisioning script, values in `.env`):

| ntfy user | role/ACL | `.env` variable |
|---|---|---|
| `kunchu01` | admin | `NTFY_ADMIN_USER`, `NTFY_ADMIN_PASSWORD` |
| `svc-portal` | read-only on the 6 topics | `NTFY_TOKEN_PORTAL` |
| `svc-alerts` | write-only `system` | `NTFY_TOKEN_ALERTS` |
| `svc-backup` | write-only `backups` | `NTFY_TOKEN_BACKUP` |
| `svc-focal` | write-only `focal` | `NTFY_TOKEN_FOCAL` |
| `svc-ritual` | write-only `ritual` | `NTFY_TOKEN_RITUAL` |
| `svc-dalal` | write-only `dalal` | `NTFY_TOKEN_DALAL` |
| `svc-general` | write-only `general` | `NTFY_TOKEN_GENERAL` |

- Portal: env `PORTAL_NTFY_URL=http://127.0.0.1:2586`, `PORTAL_NTFY_TOKEN=${NTFY_TOKEN_PORTAL}`.
  - Notifications panel: last 20 messages across the six topics (server-side poll + cache, pushed over the existing SSE hub).
  - Helper `POST /api/notify` (reachable as `http://exp.lab/api/notify`, no session needed): JSON
    `{topic, title, message, priority: urgent|normal|quiet|1..5, tags?: []}` with `Authorization: Bearer <service token>`.
    Portal validates and forwards to ntfy **with the caller's token** (Portal holds no write token; ntfy does the authz).

## Phase B: monitoring

- VictoriaMetrics single-node, `-retentionPeriod=30d`, `-httpListenAddr=127.0.0.1:8428`, memory capped, data in `./data/vm/`.
  Scrapes (`-promscrape.config`): `node` -> `127.0.0.1:9101`, `portal` -> `127.0.0.1:7080/metrics`, plus itself/vmalert/alertmanager.
- node_exporter: host network + host pid, `/` mounted ro, `--web.listen-address=127.0.0.1:9101`,
  collectors incl. `hwmon`, `thermal_zone`, `powersupplyclass`, `textfile` (`--collector.textfile.directory` = `./run/textfile` mounted ro).
  Host facts: AC adapter `ADP1` (online), battery `BAT0` (capacity/status), CPU temp = hwmon `coretemp` "Package id 0".
- Portal metric contract (existing names stay; additions):
  - `portal_app_up{app}` (exists), `portal_app_response_seconds{app}` (exists)
  - `portal_container_up{name}` 1 running / 0 otherwise, for every container in `docker ps -a`
  - `portal_container_cpu_percent{name}`, `portal_container_memory_bytes{name}` (running containers)
  - collected in the background every 30 s regardless of dashboard viewers.
- vmalert (rules in `monitoring/alerts/*.yml`) -> Alertmanager -> ntfy topic `system` (token `NTFY_TOKEN_ALERTS`, via a file under `secrets/`).
  Alertmanager: `group_by`, `group_wait 30s`, `group_interval 5m`, `repeat_interval 4h`, `send_resolved: true`.
  Urgent alerts -> ntfy priority 5, others 3, resolved 3 (use ntfy message templating or, if that proves unworkable, a Portal webhook bridge).
- Alerts: AppDown / ContainerDown (>2m, urgent; only things that were up within the last 24h, so permanently-stopped
  containers such as the bot do not nag), HighMemory (>90% 5m), DiskFull (>85%), CpuHot (>85C 5m), OnBattery (urgent),
  BatteryLow (<30%), BackupStale (no success in 26h; also fires if the metric is absent), plus PortalDown / NodeExporterDown.
- Portal UI: `/graphs` page (session required) with uPlot (vendored, embedded) fed by a Portal JSON endpoint that runs a fixed
  allowlist of named PromQL queries against `PORTAL_VM_URL=http://127.0.0.1:8428`. App tiles get a 30-day uptime bar + % (daily
  `avg_over_time(portal_app_up[1d])`, cached). VictoriaMetrics UI (`vm.exp.lab`) and Alertmanager UI (`alerts.exp.lab`) added to apps.yaml behind SSO.
- Caddy IP fix: `entrypoint.sh` becomes a tiny supervisor: it starts Caddy, re-reads the interface addresses every 15 s, and if the
  set of bind addresses changed it stops Caddy and exits so `restart: unless-stopped` brings it back on the new IPs.
  No host sysctls, no 0.0.0.0, nothing outside the Caddy container changes.

## Phase C: backups

- `backup` container (built from `backup/`): alpine + restic + rclone + sqlite + docker-cli + supercronic + tini.
  Runs as root with `cap_drop ALL` + only what reading other users' files and dropping privileges needs.
- Sources (host path -> why):
  - `~/platform/data` (portal.db WAL, apps.yaml, ntfy state; exclude `vm/`, `backup/requests`)
  - `~/focal/data` (focal.db), `~/apps/ritual/data` (ritual.db WAL), `~/apps/ledger/data` (ledger.db WAL + secret.key)
  - `~/gitea/data` (gitea.db + repositories + conf + root-owned ssh host keys; exclude sessions/log/tmp/indexers)
  - `~/dsa-tracker/dsa_tracker.db`, `~/ramayana-imax-bot/state.db`
  - Postgres: `dalal-postgres-1` (db/user `dalal`), `conveyor-postgres-1` (db/user `conveyor`) via `docker exec ... pg_dump`
    through a second socket-proxy (`docker-proxy-backup`) whose allowlist is exactly exec-create on those two containers,
    exec-start, exec-inspect, `_ping`/`version`. Plain-format uncompressed dumps (dedup-friendly).
  - Skipped: exec-engine (stopped), groqbots (ignored by request), build caches.
- Every SQLite file is copied with `sqlite3 <db> ".backup <staging>"` **run as the uid that owns the DB** (so no root-owned
  -wal/-shm files can appear next to a live DB), then `PRAGMA integrity_check` on the staged copy. restic backs up the staging
  copies plus the non-DB files; live `*.db`, `*-wal`, `*-shm` are excluded from the file walk.
- Target selection in `.env`: `BACKUP_TARGET=local` (repo `/var/backups/restic-local`) or `drive`
  (`rclone:gdrive:experimentos-restic`, config in `secrets/rclone.conf`). Switching = change that one value + `docker compose up -d backup`.
- Schedule (supercronic, `TZ=Asia/Kolkata`): backup 03:00 daily; `forget --keep-daily 7 --keep-weekly 4 --keep-monthly 6 --prune` after it;
  `restic check` Sunday 04:00; restore test on the 1st 04:30; quiet weekly summary Sunday 09:00.
- Contracts:
  - Metrics textfile `./run/textfile/backup.prom` (atomic write): `backup_last_success_timestamp_seconds`,
    `backup_last_run_timestamp_seconds`, `backup_last_run_status` (1/0), `backup_last_duration_seconds`,
    `backup_last_bytes_added`, `backup_snapshot_count`, `backup_repo_size_bytes`,
    `backup_check_last_timestamp_seconds`, `backup_check_last_status`,
    `backup_restore_test_last_timestamp_seconds`, `backup_restore_test_last_status`.
  - Status for Portal: `./data/backup/status.json` (atomic write, world-readable):
    `{target, running: ""|"backup"|"restore-staging"|"check"|"restore-test", last_run:{started,finished,status:"ok"|"failed",duration_s,bytes_added,error},
      last_success, snapshot_count, repo_size_bytes, snapshots:[{id,time}] (newest 10), last_check:{time,ok}, last_restore_test:{time,ok,files_checked,detail},
      last_staging_restore:{time,ok,snapshot,path,detail}}` (times = unix seconds).
  - Requests from Portal: a file `./data/backup/requests/<unix-nanos>.json` containing `{"action":"backup"}` or
    `{"action":"restore-staging","snapshot":"latest"|<8-64 hex>}`. A watcher in the backup container polls every 5 s, validates
    strictly, deletes the request, runs the fixed script under a lock. Nothing else is accepted. `requests/` is owned by uid 1000.
  - Restore-to-staging writes only to `/restore-staging/<UTC timestamp>-<snapshot>` = host `~/restore-staging/`; the script refuses any other target.
  - ntfy: urgent (5) on any failure to topic `backups` with `NTFY_TOKEN_BACKUP` at `http://ntfy:80`; quiet (1) weekly summary.
- Portal: Backups panel (last run, status, size, snapshots, restore-test result) + "Back up now" + "Restore to staging" buttons
  (session + Origin check like other POSTs) that only drop request files.

## Order

1. Phase A (one agent). 2. In parallel: B-infra, C-infra, Portal B+C. 3. Integration + DoD verification.
4. Security review (fable) + fixes. 5. Docs (README, LEARNING with file:line refs, MORNING.md). 6. Final commit.
