# How this works, in plain language

Each section explains one idea and points to the lines where it happens. Paths are relative to `~/platform`.

## 1. Reverse proxy

A **reverse proxy** is a server that receives requests on behalf of other servers. The browser talks only to Caddy on port 80. Caddy reads the `Host` header (`focal.exp.lab`), picks the right app and passes the request on. The app's answer travels back the same way.

Why bother:

- One port and clean names instead of `:8787`, `:8090`, `:3000`.
- One place to add things for every app at once: login, HTTPS, logging.
- Apps can listen on `127.0.0.1` only, so the proxy is the single way in.

Where:

- `caddy/Caddyfile:16` `default_bind` limits which addresses Caddy listens on. `caddy/entrypoint.sh` works them out (Tailscale IP and LAN IP).
- `caddy/Caddyfile:25` proxies the dashboard host to Portal.
- `caddy/Caddyfile:49` proxies an app. The upstream address is not written in the file. It comes from a request header that Portal set one step earlier.
- `coredns/Corefile:11` is what makes the names exist: every `*.exp.lab` lookup gets the server's Tailscale IP.

## 2. Forward auth

The apps have no idea who is logged in to Portal. Instead of teaching each app, the proxy asks a separate service before letting a request through. That is **forward auth**.

For every request to an app:

1. Caddy sends a copy of the request's headers to Portal at `/auth/verify` (`caddy/Caddyfile:43`).
2. Portal looks at the cookie and the hostname (`portal/internal/server/auth_handlers.go:30`).
3. If Portal answers `200`, Caddy continues and proxies to the app. Portal's answer includes `X-Portal-Upstream`, the app's real address (`auth_handlers.go:61`), which Caddy copies onto the request (`caddy/Caddyfile:45`).
4. If Portal answers anything else, Caddy sends that answer to the browser. A `302` becomes the redirect to the login page (`auth_handlers.go:52`). Scripts get a `401` instead, because they cannot use a login form.

Two details that matter for security:

- Caddy deletes `X-Portal-Upstream` and `X-Portal-User` from the incoming request first (`caddy/Caddyfile:34-35`). Otherwise a visitor could send those headers and choose their own upstream.
- After login, `next` decides where the browser goes. An attacker could craft a login link with `next=https://evil.example`. `SafeNext` only accepts `exp.lab` and its subdomains (`portal/internal/auth/redirect.go:21`). The suffix check with the leading dot is at `redirect.go:62`, and the `//host` trick is caught at `redirect.go:40`.

## 3. Session cookies vs JWT

After a correct password, the server must remember that this browser is logged in. Two common designs:

**Session cookie (used here).** The cookie holds a random ID that means nothing by itself. The server keeps a table of valid IDs and looks the ID up on every request.

- Created at `portal/internal/auth/session.go:68` (`CreateWithTTL`): 32 random bytes (`session.go:72`).
- Looked up at `session.go:92` (`Validate`; the query is at `session.go:99`).
- Logout deletes the row (`session.go:134`), and the cookie is worthless from that moment.
- The table stores a SHA-256 hash of the ID, not the ID (`session.go:54`, schema at `portal/internal/store/store.go:24`). A stolen database or backup contains no usable cookies.
- Sliding expiry: each use pushes the deadline 30 days out (`session.go:123`), at most once every 5 minutes to avoid a database write per request (`session.go:21`).

**JWT.** The cookie holds signed data, such as "user kunchu01, valid until Friday". The server checks the signature and needs no table. That suits many servers that share no database. The cost is that a JWT cannot be cancelled before it expires, unless you add a blocklist, which is a session table again.

With one server and one user, the lookup costs microseconds, and instant logout is worth more than statelessness.

## 4. Why argon2id

The password is never stored. A **hash** of it is: a one-way scramble. At login, the typed password is hashed the same way and the results are compared.

A fast hash (SHA-256) is the wrong tool, because an attacker who steals the database can try billions of guesses per second on a graphics card. **Argon2id** is designed to be slow and to need a lot of memory for every guess. Memory is what makes graphics cards and custom chips poor at it. The "id" variant resists both GPU cracking and timing side channels.

- Parameters: 19 MiB, 2 passes (`portal/internal/auth/password.go:35`). This is OWASP's minimum, chosen to fit the RAM budget.
- Hashing: `password.go:49`. Each hash gets a random **salt**, so two users with the same password get different hashes and precomputed tables are useless.
- The result stores the parameters and salt next to the hash, so they can be raised later without breaking old hashes.
- Verifying: `password.go:66`, ending in a constant-time comparison (`password.go:98`) whose duration does not reveal how many bytes matched.
- Only one hash runs at a time (`password.go:40`), so ten simultaneous logins cannot use 190 MiB.
- The CLI that sets the password: `portal/cmd/portal/main.go:170`.

## 5. Cookie domain scoping

A cookie normally belongs to the exact host that set it. A cookie set by `exp.lab` would not be sent to `focal.exp.lab`, and single sign-on would not work.

Setting `Domain=exp.lab` on the cookie tells the browser to send it to `exp.lab` and every subdomain (`portal/internal/server/server.go:348`). That one attribute is what makes one login cover all apps.

Browsers refuse this for "public suffixes" such as `com` or `co.uk`. Otherwise one site could set cookies for every `.com` site. The Public Suffix List is the browsers' list of those. `exp.lab` is not on it (see DECISIONS.md), so the cookie is accepted.

The other attributes:

- `HttpOnly` (`server.go:352`): JavaScript cannot read the cookie, so a script-injection bug cannot steal it.
- `SameSite=Lax` (`server.go:355`): the cookie is sent when you click a link to an app, but not on requests that other websites fire in the background.
- `Secure`: only sent over HTTPS. Off until HTTPS is added (`COOKIE_SECURE` in `.env`).

Sharing a cookie with every subdomain has a cost: every app would receive the Portal session cookie. A buggy app could leak it. So Caddy removes it from the request before proxying (`caddy/Caddyfile:52`). Apps get their own cookies and never Portal's.

`SameSite` treats `focal.exp.lab` and `exp.lab` as the same site, so it does not stop one app from posting to the dashboard. For that, every state-changing request must carry an `Origin` header equal to `http://exp.lab` (`server.go:374`).

## 6. SSE vs WebSockets

The dashboard updates without reloading. The server needs a way to push data to the browser.

**Server-Sent Events (used here).** The browser makes a normal GET request, and the server keeps the response open and writes a few lines of text whenever something changes. One direction only: server to browser.

- The handler: `portal/internal/server/dashboard.go:370`. The content type is set at `dashboard.go:378`.
- The wire format is plain text: `event:`, `data:`, blank line (`portal/internal/server/hub.go:37`).
- The hub copies each update to every open dashboard (`hub.go:65`) and gives a new one the latest state at once (`hub.go:82`).
- A comment line every 20 seconds keeps idle connections alive (`dashboard.go:405`, written at `dashboard.go:425`).
- In the browser, `EventSource` does the parsing and reconnects by itself (`portal/internal/server/web/static/app.js:146`). Phones kill background connections, so the page reconnects when it becomes visible (`app.js:173`).
- The apps table, the container list and the memory panel arrive as ready-made HTML (`app.js:163`). System numbers arrive as JSON and are patched in place (`app.js:166`).

**WebSockets** are a separate protocol with messages in both directions. They suit chat or games. A dashboard only listens, and the one action it has (restart) is an ordinary POST (`dashboard.go:432`). SSE is less code, works through any HTTP proxy and reconnects for free.

## 7. Rate limiting

Without a limit, a script could try thousands of passwords a minute. With **exponential backoff**, each failure after the third doubles the wait: 2s, 4s, 8s, up to 15 minutes.

- The limiter: `portal/internal/auth/ratelimit.go`. `Allow` at line 47, `Fail` at line 67, the doubling at line 85, reset on success at line 95.
- It is checked before the password is looked at (`portal/internal/server/auth_handlers.go:115`). A locked-out client cannot test a guess at all, even the right one.
- A failure is recorded at `auth_handlers.go:157`.
- Clients are told apart by IP address (`portal/internal/server/server.go:381`). Caddy supplies the real address in `X-Forwarded-For`.

## 8. Smaller ideas worth knowing

- **Least privilege for Docker.** Access to the Docker socket is root on the host. Portal never gets it. A separate proxy forwards only "list", "stats", "restart" and "stop" (`docker-compose.yml:140-141`) and has no network at all (`docker-compose.yml:142`). Its socket is in `run/portal/`, the only part of `run/` Portal mounts (`docker-compose.yml:93`). Portal's client: `portal/internal/docker/docker.go:66` (HTTP over a unix socket), `docker.go:110` (list), `docker.go:151` (restart).
- **Reading the machine.** CPU, memory, battery and WiFi come from kernel pseudo-files, with no tools run: `portal/internal/sysinfo/sysinfo.go:85` (CPU from `/proc/stat`), `:125` (memory), `:167` (temperature), `:206` (WiFi), `:232` (battery from `/sys/class/power_supply`).
- **Health checks.** Every 30 seconds each app is requested directly (`portal/internal/apps/apps.go:218`). Any answer below HTTP 500 counts as up.
- **Config hot reload.** `apps.yaml` is re-read when its modification time changes. A broken edit keeps the previous version (`apps.go:141`).
- **Content-Security-Policy.** A response header that tells the browser to run scripts only from this site (`portal/internal/server/server.go:302`).
- **SQLite in WAL mode.** Lets the CLI write while the server reads (`portal/internal/store/store.go:40`).
- **Metrics.** `/metrics` is plain text in Prometheus format, written by hand (`portal/internal/server/metrics.go:121`; more in section 10).
- **Tests.** `portal/internal/auth/*_test.go` cover hashing, session expiry, redirect validation, the rate limiter and TOTP. They run on every image build (`portal/Dockerfile`), so a failing test stops a deploy.

---

Sections 9 to 19 cover phase 2 (12–13 Nov 2026): notifications, monitoring and backups. Line numbers were checked against the code on 13 Nov 2026.

## 9. Pub/sub and push notifications

**Publish/subscribe** ("pub/sub") separates whoever sends a message from whoever reads it. A **publisher** sends a message to a **topic**, which is just a name such as `backups`. A **subscriber** asks for the messages of one or more topics. Neither knows the other exists; the topic name is the only thing they share. So adding a sender (a new app) or a reader (a second phone) changes nothing for the others.

In ntfy, publishing is one HTTP request: `POST http://ntfy.exp.lab/backups` with the text as the body (`ntfy/server.yml:4`). Who may do what is decided per user and topic:

- Anonymous users may do nothing (`ntfy/server.yml:61`, `deny-all`).
- Each service user may write exactly one topic and read none; Portal's user may read all six and write none (`compose.notify.yml:66`, for example `svc-alerts:system:wo` at `compose.notify.yml:69`).
- The six topics are named once in Go (`portal/internal/notify/notify.go:34`).

**Three ways to receive.** A subscriber can:

1. **Poll**: ask "anything new since X?" now and then and get an answer that ends. Portal does this every 15 seconds (`portal/internal/server/dashboard.go:623`) with one request for all six topics, `GET /system,backups,.../json?poll=1&since=<last id>` (`portal/internal/notify/notify.go:151`). Simple and robust, but up to 15 seconds late, and every poll costs a request even when nothing happened.
2. **Stream**: open one request that never ends, and the server writes each message down it as it happens. Instant and cheap while idle, but the connection must stay open (NOTIFY.md shows `curl -sN .../system/json`).
3. **Push**: a third party wakes the device when there is something to fetch. Phones need this, because iOS kills background connections to save battery. Only Apple's push service (**APNs**) may wake an app, and only the holder of an app's APNs key may send to it. For the ntfy app, that is the public `ntfy.sh` server.

**How the iPhone gets woken without seeing the content.** With `upstream-base-url: https://ntfy.sh` (`ntfy/server.yml:103`, explained at `ntfy/server.yml:90`), for each new message our server sends ntfy.sh a "poll request" containing only the message ID. The topic it uses there is the SHA-256 hash of our topic URL, not the name. ntfy.sh passes it through APNs, the app wakes, and the app fetches the real message from `http://ntfy.exp.lab` over Tailscale with your login. ntfy.sh learns that some message exists for some hashed topic at some time. Nothing else.

Two more pub/sub details in this stack:

- **Senders that cannot speak ntfy.** Alertmanager can only POST its own JSON. ntfy can apply a template to that JSON when the URL says `?template=alert-urgent` (`monitoring/alertmanager.yml:54`). The template sets title, text and priority: 5 while firing, 3 when resolved (`ntfy/templates/alert-urgent.yml:40`).
- **The cache.** ntfy keeps messages for 7 days (`ntfy/server.yml:49`). A phone that was offline asks "since my last message" and catches up. That is why polling with `since=` works.

**Portal's SSE hub is a small pub/sub of its own** (section 6). Background loops publish named events (`apps`, `containers`, `notify`, `backup`); each open dashboard tab is a subscriber.

- `Publish` sends to every subscriber, skipping a message identical to the last one for that event (`portal/internal/server/hub.go:57`).
- It never waits for a slow subscriber: if a tab's buffer is full, that tab misses this update and gets the next full state instead (`portal/internal/server/hub.go:70`).
- `Subscribe` hands a new tab the latest message of every event at once (`portal/internal/server/hub.go:78`), like ntfy's cache.
- Notification text comes from outside, so before it is framed as an event every `\r` is turned into `\n`. Otherwise a message could start a line of its own that the browser reads as a separate SSE field and forge an event (`portal/internal/server/hub.go:40`, inside `formatEvent` at `hub.go:37`). This was a security-review fix (section 17 has another one).
- The Notifications panel is published to this hub after each ntfy poll (`portal/internal/server/notify.go:122`) and right after a successful `/api/notify` (`portal/internal/server/notify.go:202`).

## 10. Pull-based metrics and time-series databases

A **metric** is a number with a name, for example "RAM available". A **time series** is that number recorded again and again: a list of (time, value) pairs. A **time-series database** (TSDB) is built to store millions of such pairs cheaply and answer questions like "average over the last hour".

**Pull, not push.** Programs here do not send their numbers anywhere. Each one serves a plain-text page, `/metrics`, with its current values. VictoriaMetrics fetches ("scrapes") every page every 30 seconds (`monitoring/scrape.yml:34`) and stores each line with the time it read it. The comment at `portal/internal/server/metrics.go:17` explains the upside: when a program dies, the scrape fails and the scraper records `up = 0`. A push-based program that dies just goes quiet, and silence looks the same as "nothing to report".

**The exposition format.** Portal writes the page by hand (`portal/internal/server/metrics.go:121`):

```
# HELP portal_app_up Whether the app's last health check succeeded.
# TYPE portal_app_up gauge
portal_app_up{app="focal"} 1
```

- `HELP` and `TYPE` are comments for humans and tools. A **gauge** is a reading that goes up and down (temperature, "is it up"). A **counter** only goes up (requests served since start) and is meaningful only as a rate (`metrics.go:33`).
- `{app="focal"}` is a **label**. The name plus the full set of labels identifies one series: `portal_app_up{app="focal"}` and `portal_app_up{app="git"}` are two series.
- Label values must escape `\`, `"` and newline (`metrics.go:55`).
- A counter that only appears after its first increment makes `rate()` miss that first event, so Portal prints every combination from the start, zeros included (`metrics.go:158`).
- `portal_container_up` is printed for **every** container, stopped ones as `0` (`portal/internal/server/containers.go:143`, the line itself at `containers.go:150`). "A container stopped" is then a value changing from 1 to 0, which a rule can see. If stopped containers were left out, the series would just stop, and missing data is much harder to alert on.

**Scrape config.** `monitoring/scrape.yml` lists **jobs** (`node` at `scrape.yml:41`, `portal` at `scrape.yml:48`), each with **targets** (addresses). Every series gets `job` and `instance` labels, and the scraper adds an `up` series per target.

**Cardinality** is the number of distinct series. Each one costs memory and disk, so labels must have few possible values (`monitoring/scrape.yml:28`). Never use a user ID, a URL or a timestamp as a label. Two choices here keep it small:

- node_exporter starts with every collector off and switches on only the ones used (`compose.monitoring.yml:61`).
- Docker's virtual network interfaces (`veth…`) are excluded (`compose.monitoring.yml:78`). They change with every container restart and would create new series each time.

Result on 13 Nov: about 3,500 series in total, 24 of them `portal_container_up`, and 5.7 MB on disk after the first 8 hours.

**Instant and range queries.** PromQL is the query language. Portal's client (`portal/internal/metricsdb/metricsdb.go`) has two calls:

- `Query` (`metricsdb.go:119`): evaluate an expression at one moment, giving one value per series. Example: "uptime so far today".
- `QueryRange` (`metricsdb.go:131`): evaluate the same expression at start, start+step, start+2·step… That is one line on a chart. The **step** sets the chart's resolution, not the scrape interval. `/graphs` picks the step so every range draws about 360 points (`portal/internal/server/graphs.go:37`): 10 s for 1 hour, 2 h for 30 days.

**`rate()`.** A counter like `node_cpu_seconds_total` only grows. `rate(x[5m])` takes the samples of the last 5 minutes and returns the increase per second. CPU busy % is then `100 - idle rate × 100` (`graphs.go:78`). The window must be at least as long as the step, otherwise a 2-hour step with a 5-minute window looks at only 5 minutes of every 120 (`graphs.go:52`, `rateWindow` at `graphs.go:225`).

**Retention.** Old data is deleted automatically after 30 days (`compose.monitoring.yml:107`). The uptime bars therefore show 30 days.

**Why VictoriaMetrics and not Prometheus.** Both speak the same scrape format and PromQL. Prometheus is the reference implementation, but it keeps the last hours of samples in memory and usually needs noticeably more RAM than VictoriaMetrics for the same data. On an 8 GB laptop with a 350 MiB budget for everything new, VictoriaMetrics fits: the scraper, storage and query UI are one process, its cache budget can be capped (`compose.monitoring.yml:113`, with `GOMEMLIMIT` at `compose.monitoring.yml:125`), and it compresses samples to well under a byte each. It runs at about 90–115 MiB here.

## 11. Alert timing: `for:`, grouping, repeats and flapping

An alert rule is a query that should normally return nothing. vmalert runs every rule every 30 seconds (`compose.monitoring.yml:169`). Each series the query returns is one alert, identified by its labels (`monitoring/alerts/services.yml:6`):

- **pending**: returned for the first time. Nothing is sent (`services.yml:10`).
- **firing**: still returned after the rule's `for:` time (`services.yml:48` for ContainerDown: 2 minutes). Sent to Alertmanager.
- **resolved**: no longer returned. Alertmanager sends a "Resolved" message (`send_resolved` at `monitoring/alertmanager.yml:55`).

`for:` filters blips: a container that restarts in 10 seconds, or one failed scrape, never reaches the phone. vmalert stores its timers in VictoriaMetrics (`compose.monitoring.yml:175`), so restarting vmalert does not reset a timer that was 90 seconds in.

**Alertmanager turns a stream into a few messages.** vmalert re-sends every firing alert every 30 seconds. Alertmanager decides when you hear about it:

- `group_by: [alertname]` (`monitoring/alertmanager.yml:22`): all ContainerDown alerts share one message, so five containers dying in a reboot is one buzz.
- `group_wait: 30s` (`alertmanager.yml:27`): after the first alert of a new group, wait 30 s for others to join.
- `group_interval: 5m` (`alertmanager.yml:31`): after a message for a group, wait at least 5 minutes before the next one for that group, whether an alert joined or resolved.
- `repeat_interval: 4h` (`alertmanager.yml:35`): if nothing changed but it is still firing, remind every 4 hours.

**Worked example: the test run on 12 Nov (times UTC; ≈ marks times worked out from the intervals, the others were observed).**

| Time | What happened | Why then |
|---|---|---|
| 20:04:03 | A dummy container is stopped | |
| ≈20:04:30 | `portal_container_up{name="dummy"}` becomes 0 in VictoriaMetrics; the rule returns it: **pending** | Portal reads Docker every 10–30 s, VictoriaMetrics scrapes every 30 s, vmalert evaluates every 30 s |
| ≈20:06:55 | Still down after 2 minutes: **firing** | `for: 2m` |
| 20:07:25 | One notification, priority 5 | `group_wait: 30s` |
| 20:07:34 | The container is started again; at the next evaluations the alert is **resolved** | |
| 20:12:25 | One "Resolved" notification, priority 3 | exactly 5 minutes after the firing message: `group_interval: 5m` |

The resolve took 5 minutes because, for Alertmanager, a resolve is an update to a group, and a group gets at most one update per `group_interval`. A shorter interval would mean faster "Resolved" messages but also more messages when something flaps.

**Flapping** is a value hovering around the threshold (disk at 84.9 %, 85.1 %, 84.9 %…), which would fire, resolve, fire… (`monitoring/alerts/host.yml:5`). Three guards:

1. `for:` stops the "fire" side.
2. `keep_firing_for: 15m` on DiskFull (`host.yml:47`) stops the "resolve" side: once firing, the alert stays firing until the value has been below the line for 15 minutes.
3. `group_interval` caps it at one message per 5 minutes in any case.

**Only things that were recently up.** ContainerDown also requires that the container was up at some point in the last 24 hours, `max_over_time(portal_container_up[24h]) == 1` (`services.yml:47`). Containers that have been stopped for weeks (the bots) never alert. The side effect: a container that stays down for more than a day resolves by itself.

## 12. Dead man's switch

A train's dead man's switch stops the train when the driver *stops* pressing it. Monitoring has the same problem: a job that fails can send "I failed", but a job that never runs (container gone, cron broken, machine off) sends nothing, and nothing looks exactly like "all good".

So the backup proves it is alive instead, and the alert fires when the proof is too old or missing (`monitoring/alerts/backup.yml:3`):

- After every successful backup, `last_success` is set to the finish time (`backup/scripts/backup.sh:210`).
- `write_metrics` turns that into `backup_last_success_timestamp_seconds` (`backup/scripts/lib.sh:334`, the metric at `lib.sh:341`) in `run/textfile/backup.prom`. node_exporter's textfile collector serves it and VictoriaMetrics scrapes it like any other metric.
- BackupStale (`monitoring/alerts/backup.yml:24`) fires when `time() - backup_last_success_timestamp_seconds > 26 * 3600` (`backup.yml:26`): the daily run plus 2 hours of slack.
- `or on() absent(backup_last_success_timestamp_seconds)` (`backup.yml:28`) covers the metric not existing at all. `absent()` returns a series exactly when there is none. Without this half, a deleted file or a broken collector would make the first half return nothing, and the alert would stay silent.
- **Staleness**: when a scrape no longer contains a series, the database marks it stale and queries stop returning it (at the latest after the few-minute lookback window). So "the file disappeared" becomes "absent" within minutes, and `for: 10m` (`backup.yml:29`) then fires.

This is also why a successful backup sends no message (`backup/scripts/backup.sh:214`): a daily "OK" trains you to ignore the topic. Silence is normal; the switch catches silence that lasts too long.

## 13. 3-2-1 backups

The rule of thumb: **3** copies of your data, on **2** different kinds of storage, **1** of them off-site.

A **copy** is something you can restore from when the original is gone. That rules out:

- **RAID or a second partition on the same disk**: one hardware failure, theft or fire takes both.
- **A sync folder**: it faithfully copies a deletion or a corruption too.
- **A backup you cannot decrypt**: without `RESTIC_PASSWORD` the repository is random bytes.

Here:

1. The live data (copy 1).
2. The local restic repository `/var/backups/restic-local` (`compose.backup.yml:99`, chosen at `backup/scripts/lib.sh:148`). It has history and protects against mistakes: a deleted file, a broken upgrade, a corrupt database. It is on **the same disk**, so against a dead disk, theft or fire it counts for nothing.
3. Google Drive (`lib.sh:151`): another company's storage in another place. That is the off-site copy, and it starts once you connect it (RESTORE.md section 8).

`select_repo` (`lib.sh:143`) uses one target at a time. Today that is the local repository; after the switch it is Drive. Once you have switched, you have live data plus Drive, plus the frozen local repository holding history up to the switch.

## 14. Incremental and deduplicated backups

A naive backup copies everything every night: 30 nights of a 30 MB data set is 900 MB. restic stores each piece of data once.

**Content-defined chunking.** restic cuts every file into chunks of about 0.5–8 MB. It does not cut at fixed offsets: it slides a small window over the bytes and cuts wherever a rolling hash of the window matches a pattern. The cut points depend on the content, so inserting a line near the start of a file moves only the nearby cut and the later chunks stay identical. Each chunk is named by its SHA-256 hash and stored once; a chunk that already exists costs nothing.

**Snapshots are trees.** A snapshot is a small object pointing to a tree of directories. Each file entry lists its chunk hashes. Two snapshots of an unchanged file point to the same chunks. Every snapshot is a complete picture you can restore on its own; there is no chain of "full + incrementals". Deleting a snapshot (`forget`) removes the pointers, and `prune` deletes chunks nothing points to any more (`backup/scripts/backup.sh:197`: keep 7 daily, 4 weekly, 6 monthly).

**Why the Postgres dumps are plain, uncompressed SQL** (`backup/scripts/backup.sh:86`). Change one row and a plain dump changes in a few lines, so most chunks stay identical. A gzip-compressed dump changes almost completely after the first difference, because compression carries state forward through the file. restic would store it in full every night. restic compresses its chunks itself, so nothing is lost by dumping plain.

**Measured on 12–13 Nov:**

- First backup: 29.4 MB scanned, 5.2 MB stored (compression plus dedup inside the first run).
- Second backup: 1.69 MB stored. Only the changed chunks were new.
- A later run: 29.5 MB scanned, 2.46 MB stored. Ritual's WAL had grown, and the SQLite copies change a little each time.
- The repository was 9.8 MB for two snapshots.

The summary line comes from `backup/scripts/backup.sh:189`.

**Checking without reading everything.** `restic check` verifies the structure: every snapshot points to trees and chunks that exist. `--read-data-subset=n/10` also downloads and verifies one tenth of the data, rotating with the week number (`backup/scripts/check.sh:23`, `check.sh:26`). Everything is read every 10 weeks, and a weekly check on Drive stays cheap.

## 15. Why a live SQLite file cannot simply be copied

A SQLite database is a file of fixed-size **pages** (4 KB). A write changes several pages, and the file is only consistent between transactions.

- **A torn copy.** `cp` reads the file from start to end while the app keeps writing. The copy can contain page 10 from before a transaction and page 900 from after it. That file may fail to open, or open and return wrong data.
- **Journal modes.** In the default *rollback journal* mode, SQLite saves the old pages to `app.db-journal` before overwriting them in `app.db`. In **WAL** (write-ahead log) mode, new pages are appended to `app.db-wal` instead, and `app.db` itself is only updated at a **checkpoint**, when the WAL is copied back. Until then, the newest data exists **only in the -wal file**.
- **Concrete example.** When the backups were built, Ritual's `ritual.db` was 61 KB and `ritual.db-wal` 2.6 MB (3.4 MB on 13 Nov). Copying only `ritual.db` would lose most of what you entered recently. Copying both files at slightly different moments gives a pair that does not match.

**What `.backup` does.** `sqlite3 app.db ".backup copy.db"` uses SQLite's online backup API. It reads the database through SQLite under a read lock, which includes the WAL content, and writes a single self-contained file. The app can keep reading and writing meanwhile. In this stack:

- The copy is made at `backup/scripts/backup.sh:50`, with `.timeout 10000` so it waits up to 10 s if the app holds a write lock.
- It is then checked with `PRAGMA integrity_check` (`backup.sh:53`). Only `ok` passes.
- The raw `*.db`, `-wal` and `-shm` files are excluded from restic's file walk; only these copies are stored.

**Why as the database owner.** Even a reader of a WAL database writes to it: it must update the shared-memory index `app.db-shm`, and opening may create `-wal`/`-shm`. If root did that, a root-owned `-shm` would appear next to Ritual's database, and Ritual (uid 1000) could no longer open its own database. So the script reads the owner of each file (`backup.sh:45`) and runs sqlite3 as that uid/gid with `setpriv` (`backup/scripts/lib.sh:99`). This needs the `SETUID`/`SETGID` capabilities (`compose.backup.yml:36`) and read-write mounts on the directories with live databases (`compose.backup.yml:79`). Both are listed as accepted risks in DECISIONS.md.

## 16. Restore tests

"A backup you have not restored is a hope" (`backup/scripts/restore-test.sh:6`). Some problems only appear when you restore:

- a wrong or lost password
- a repository with missing pieces
- a database copied mid-write
- a dump cut off halfway
- a folder you assumed was included but an exclude rule skipped

So the restore is tested for real:

- **Monthly** (`backup/crontab:16`, the 1st at 04:30 IST): `restore-test.sh` unpacks the newest snapshot into a scratch directory (`restore-test.sh:47`). It runs `PRAGMA integrity_check` on every SQLite file it finds by content, not by name (`check_sqlite_tree`, `backup/scripts/lib.sh:119`; called at `restore-test.sh:50`), and fails if any check fails (`restore-test.sh:52`). It also checks that every expected database is present and that both Postgres dumps end with pg_dump's "dump complete" marker. Then it deletes the scratch copy.
- **Weekly**: `restic check` (section 14).
- **On demand**: "Restore to staging" on the dashboard does the same into `~/restore-staging/` (`backup/scripts/restore-staging.sh:55`), where you can look at it.

On 12–13 Nov, a staging restore and a restore test passed: 9 of 9 SQLite files `ok`, both dumps complete. A deliberately broken run produced the urgent "FAILED" message, so the failure path is tested too.

## 17. Trust boundaries: the status.json bug

A **trust boundary** is where data crosses from a less trusted place into a more trusted one. Code on the trusted side must treat that data as hostile, however it got there.

`data/backup/status.json` is written by the backup container (root), but it lives in a directory that uid 1000, and therefore Portal, can write. The weekly summary script read `last_success` from it and used it in bash arithmetic, `$((now - last_success))`. Bash evaluates array subscripts inside arithmetic, so a value like `a[$(command)]` **runs the command**, here as root in the backup container. Anyone who could write that file as uid 1000 could have become root in a container that can reach the Docker socket proxy.

The fix (commit f525cbd) treats the file as untrusted input at every read:

- `backup/scripts/lib.sh:251` explains the threat. `JQ_SANITIZE` (`lib.sh:261`) rebuilds the object from the schema: numbers must be non-negative integers or become `null`, strings are length-capped, snapshot IDs must be hex, unknown keys are dropped. `status_read` applies it (`lib.sh:291`).
- `is_uint` (`lib.sh:302`) accepts only plain digits, no sign and no leading zero (bash reads `010` as octal). `status_num` (`lib.sh:307`) returns a number or nothing.
- `weekly-summary.sh` reads `last_success` only through `status_num` (`backup/scripts/weekly-summary.sh:20`). The arithmetic still checks `is_uint` and forces base 10 (`weekly-summary.sh:37`). Two independent checks: defence in depth.

The lesson: a file is an input, like an HTTP request. Where it is stored decides who could have written it.

## 18. Atomic file writes

Several files here are written by one process and read by another at any moment. If the writer is halfway through, the reader sees half a file: invalid JSON, a truncated metrics file, a request with a missing field.

The fix is always the same. Write to a temporary file **in the same directory**, then `rename()` it over the real name. On Linux, a rename within one filesystem is **atomic**: a reader sees either the whole old file or the whole new one, never a mix.

- status.json and backup.prom: `write_file_as_owner` (`backup/scripts/lib.sh:230`), which writes and then does `mv -f` (`lib.sh:234`).
- Portal's backup requests: `WriteRequest` (`portal/internal/backup/backup.go:288`) creates a temp file (`portal/internal/backup/backup.go:296`) and renames it into place (`portal/internal/backup/backup.go:326`). The watcher only picks up `*.json` names, so it never sees the temp file.
- The Alertmanager token file: `scripts/monitoring-secrets.sh:36` (`mktemp`) and `:45` (`mv`).

The same directory matters because a rename across filesystems is a copy plus a delete, which is not atomic.

## 19. Least privilege in the two socket proxies

Access to `/var/run/docker.sock` is root on the host: whoever can talk to it can start a privileged container that mounts `/`. So nothing here talks to it directly. Two separate proxies each allow a different, minimal set of calls (section 8 has the first one):

- **Portal's proxy** allows `GET` container list and stats, and `POST` restart and stop (`docker-compose.yml:140-141`). No network at all (`docker-compose.yml:142`). Its socket is in `run/portal/`, and that is the only part of `run/` Portal mounts (`docker-compose.yml:93`).
- **The backup proxy** allows only what `docker exec … pg_dump` needs in the two Postgres containers: inspect them, create an exec, start it, read its exit code, ping (`compose.backup.yml:168`, `compose.backup.yml:170`). Its socket is mode 600 and owned by root (`compose.backup.yml:159`), in a named Docker volume that only these two containers mount (`compose.backup.yml:112`, `compose.backup.yml:182`). Not even uid 1000 on the host can reach it.

**The limit, stated in the file** (`compose.backup.yml:129`): the proxy filters only the method and path of a request, never its body. The body of an exec-create request chooses the command and can ask for `"Privileged": true`. So whoever controls the backup container can run anything as root in those two Postgres containers, and from there reach the host. Least privilege made the door narrow. It is still a door. The rest of the defence is around it: no listening ports, three capabilities, strict validation of Portal's request files (`backup/scripts/watcher.sh:67`), sanitised status (section 17), and children started with an empty environment (`backup/scripts/lib.sh:103`), so sqlite3 or tar never inherit the restic password or ntfy token.

## 20. Where does the RAM go? RSS, PSS, page cache and cgroups

`free` says "used 2.7 GB" but not *by whom*. The Memory panel answers that by splitting `MemTotal` into groups that add up, and the parts are worth understanding one by one.

**Page cache is not "used".** Linux keeps the contents of recently read files in RAM, because spare RAM is wasted RAM. When a program asks for memory, the kernel drops clean cache pages instantly. That is why `free` shows a big `buff/cache` and a much bigger `available` than `free`. The panel puts `Cached + Buffers + SReclaimable - Shmem` in one "Page cache" group (`portal/internal/procmem/procmem.go:372`) and says in plain words that it is lent, not taken.

**RSS counts shared pages more than once.** `VmRSS` in `/proc/<pid>/status` is every page a process has in RAM: its own heap (`RssAnon`), mapped files such as its binary and libraries (`RssFile`), and shared memory (`RssShmem`). Ten processes using the same library each count it. Summing RSS over all processes therefore says more than the machine has. Two ways out:

- **PSS** (proportional set size, in `/proc/<pid>/smaps_rollup`) divides each shared page by the number of processes sharing it, so the sum is honest. Reading it needs ptrace-level access. Docker's AppArmor profile refuses that for processes outside the container, so Portal does not use it.
- **Leave file pages out.** `RssFile` pages already sit in the page cache group. Charging a process only `RssAnon + RssShmem` (`procmem.go:513`) avoids counting them twice. On this machine the sum over all processes matches meminfo's `AnonPages + Shmem` within about 1%. Portal uses this.

**cgroups tell you which container a process is in.** `/proc/<pid>/cgroup` shows the cgroup path, for example `0::/system.slice/docker-<64 hex>.scope`. The id in it is the container id from the Docker API, which also carries the compose project label (`procmem.go:157`, matched at `procmem.go:241`). Portal sums the processes of each container itself rather than using `docker stats`. Docker's figure includes the container's *active* page cache, which is already in the cache group.

**Groups come from the process tree.** A `claude` process and all of its descendants form one Claude Code session (`procmem.go:221`). `dockerd`, `containerd` and the shims are the Docker engine (`procmem.go:271`). Everything else is split by uid into your own processes and system services (`procmem.go:281`). The kernel's own memory (unreclaimable slab, page tables, stacks, vmalloc) comes straight from `/proc/meminfo` (`procmem.go:360`).

**Be honest about the remainder.** Whatever is left is reported as "unaccounted" (`procmem.go:385`): memory the kernel holds that no file attributes (graphics buffers, drivers), minus anonymous pages shared after `fork()` and counted twice. It is usually around 1% here. If it ever went negative, the panel would show a negative number instead of hiding it.

**Reading /proc safely and cheaply.** Processes come and go while you read, so every file is read once and any error drops only that process (`procmem.go:169`). Kernel threads have no `VmRSS` line and are skipped (`procmem.go:487`). A scan of ~140 processes takes about 4 ms (12 ms inside the container). It runs every 10 s while a dashboard is open and every 60 s otherwise (`portal/internal/server/dashboard.go:574`, `portal/internal/server/memory.go:44`). Only nine low-cardinality series leave the process (`memory.go:297`). Per-pid series would create a new time series for every short-lived process.

**Command lines leak secrets.** `/proc/<pid>/cmdline` is world-readable, and tools still put tokens there. Before anything reaches the page, `Sanitize` (`procmem.go:679`) keeps only the program's base name and replaces values after `--token`, `--password`, `-p`, `-H`, `KEY=...`, `Authorization`/`Bearer` and URL credentials with `***`.

Further reading: `man 5 proc` (the `status`, `smaps_rollup` and `meminfo` entries); the kernel's `Documentation/admin-guide/cgroup-v2.rst`.

## 21. Stopping things on purpose: the stop flow and alert silences

A monitoring system cannot tell "it crashed" from "I switched it off". If you stop the bots from the dashboard, the ContainerDown rule (`monitoring/alerts/services.yml`) fires two minutes later and your phone rings. The fix is to tell Alertmanager first, with a **silence**: a stored rule "do not notify for alerts whose labels match X until time T". The alert still fires in vmalert (you can see it in Alertmanager as *suppressed*); it just never reaches ntfy.

**The flow, one request.** `POST /api/apps/bot/stop` (`portal/internal/server/control.go:234`):

1. Same gate as restart: a live session, then `Origin` must be exactly `http://exp.lab` (`control.go:54`), so a page on another site or a compromised sibling app cannot press the button.
2. Resolve the names from `apps.yaml` against the live container list (`control.go:99`). An unknown name or a platform container stops the whole request with 409. Nothing typed in a file or a URL ever goes to Docker unless it matched a container Docker itself listed.
3. Refuse the lifeline containers (`control.go:48`): Portal, Caddy, CoreDNS and the socket proxies, matched by compose labels (`project=platform` + service), not by a name substring. Stopping Portal from Portal would leave no button to start it again.
4. `docker stop` through the socket proxy (`portal/internal/docker/docker.go:162`), which now allows `restart|stop` and still refuses start, kill, exec, create and delete (`docker-compose.yml:141`). "Start" is Docker's restart of a stopped container (`control.go:266`).
5. Silence `name="<container>"` and `app="bot"` for 30 days, created by `portal` (`control.go:279`, `silences.go:74`). If Alertmanager is unreachable the stop has already happened; the page shows a warning instead of failing (`control.go:152`).

**Ending a silence without a stale alert.** On Start, Portal does not delete the silence at once. vmalert still reports the container as down for up to a minute (30 s scrape + 30 s evaluation), and Alertmanager flushes each alert group every 5 minutes; a silence deleted too early lets that stale ContainerDown through. So Portal moves the silence's end to 3 minutes from now (`silences.go:178`, `silences.go:44`). That change is stored in Alertmanager, so it survives a Portal restart, unlike a timer in memory.

**Memory vs. the source of truth.** Portal remembers the silence ids, but Alertmanager's list is what counts: on Start it looks up active silences with `createdBy=portal` and exactly that one matcher (`silences.go:309`), so a silence made before a Portal restart is still found, and someone else's silence on the same container is left alone. The same check stops a second Stop from piling up duplicates (`silences.go:81`).

**Quiet when off on purpose.** The app row says "stopped" in plain ink, not red "down" (`portal/internal/server/dashboard.go:235`), and the status line counts silenced alerts apart (`portal/internal/server/status.go:158`).

Further reading: Alertmanager docs, "Silences" and the v2 API (`/api/v2/silences`).
