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

- Created at `portal/internal/auth/session.go:60`: 32 random bytes.
- Looked up at `session.go:81`.
- Logout deletes the row (`session.go:118`), and the cookie is worthless from that moment.
- The table stores a SHA-256 hash of the ID, not the ID (`session.go:54`, schema at `portal/internal/store/store.go:24`). A stolen database or backup contains no usable cookies.
- Sliding expiry: each use pushes the deadline 30 days out (`session.go:107`), at most once every 5 minutes to avoid a database write per request (`session.go:21`).

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
- The CLI that sets the password: `portal/cmd/portal/main.go:152`.

## 5. Cookie domain scoping

A cookie normally belongs to the exact host that set it. A cookie set by `exp.lab` would not be sent to `focal.exp.lab`, and single sign-on would not work.

Setting `Domain=exp.lab` on the cookie tells the browser to send it to `exp.lab` and every subdomain (`portal/internal/server/server.go:229`). That one attribute is what makes one login cover all apps.

Browsers refuse this for "public suffixes" such as `com` or `co.uk`. Otherwise one site could set cookies for every `.com` site. The Public Suffix List is the browsers' list of those. `exp.lab` is not on it (see DECISIONS.md), so the cookie is accepted.

The other attributes:

- `HttpOnly` (`server.go:233`): JavaScript cannot read the cookie, so a script-injection bug cannot steal it.
- `SameSite=Lax` (`server.go:236`): the cookie is sent when you click a link to an app, but not on requests that other websites fire in the background.
- `Secure`: only sent over HTTPS. Off until HTTPS is added (`COOKIE_SECURE` in `.env`).

Sharing a cookie with every subdomain has a cost: every app would receive the Portal session cookie. A buggy app could leak it. So Caddy removes it from the request before proxying (`caddy/Caddyfile:52`). Apps get their own cookies and never Portal's.

`SameSite` treats `focal.exp.lab` and `exp.lab` as the same site, so it does not stop one app from posting to the dashboard. For that, every state-changing request must carry an `Origin` header equal to `http://exp.lab` (`server.go:254`).

## 6. SSE vs WebSockets

The dashboard updates without reloading. The server needs a way to push data to the browser.

**Server-Sent Events (used here).** The browser makes a normal GET request, and the server keeps the response open and writes a few lines of text whenever something changes. One direction only: server to browser.

- The handler: `portal/internal/server/dashboard.go:251`. The content type is set at `dashboard.go:259`.
- The wire format is plain text: `event:`, `data:`, blank line (`portal/internal/server/hub.go:30`).
- The hub copies each update to every open dashboard (`hub.go:47`) and gives a new one the latest state at once (`hub.go:68`).
- A comment line every 20 seconds keeps idle connections alive (`dashboard.go:306`).
- In the browser, `EventSource` does the parsing and reconnects by itself (`portal/internal/server/web/static/app.js:65`). Phones kill background connections, so the page reconnects when it becomes visible (`app.js:88`).
- App tiles and the container list arrive as ready-made HTML (`app.js:81`). System numbers arrive as JSON and are patched in place so the bars animate (`app.js:83`).

**WebSockets** are a separate protocol with messages in both directions. They suit chat or games. A dashboard only listens, and the one action it has (restart) is an ordinary POST (`dashboard.go:313`). SSE is less code, works through any HTTP proxy and reconnects for free.

## 7. Rate limiting

Without a limit, a script could try thousands of passwords a minute. With **exponential backoff**, each failure after the third doubles the wait: 2s, 4s, 8s, up to 15 minutes.

- The limiter: `portal/internal/auth/ratelimit.go`. `Allow` at line 47, `Fail` at line 67, the doubling at line 85, reset on success at line 95.
- It is checked before the password is looked at (`portal/internal/server/auth_handlers.go:115`). A locked-out client cannot test a guess at all, even the right one.
- A failure is recorded at `auth_handlers.go:157`.
- Clients are told apart by IP address (`portal/internal/server/server.go:262`). Caddy supplies the real address in `X-Forwarded-For`.

## 8. Smaller ideas worth knowing

- **Least privilege for Docker.** Access to the Docker socket is root on the host. Portal never gets it. A separate proxy forwards only "list", "stats" and "restart" (`docker-compose.yml:86-87`) and has no network at all (`docker-compose.yml:88`). Portal's client: `portal/internal/docker/docker.go:54` (HTTP over a unix socket), `docker.go:86` (list), `docker.go:160` (restart).
- **Reading the machine.** CPU, memory, battery and WiFi come from kernel pseudo-files, with no tools run: `portal/internal/sysinfo/sysinfo.go:83` (CPU from `/proc/stat`), `:125` (memory), `:167` (temperature), `:206` (WiFi), `:232` (battery from `/sys/class/power_supply`).
- **Health checks.** Every 30 seconds each app is requested directly (`portal/internal/apps/apps.go:218`). Any answer below HTTP 500 counts as up.
- **Config hot reload.** `apps.yaml` is re-read when its modification time changes. A broken edit keeps the previous version (`apps.go:141`).
- **Content-Security-Policy.** A response header that tells the browser to run scripts only from this site (`portal/internal/server/server.go:183`).
- **SQLite in WAL mode.** Lets the CLI write while the server reads (`portal/internal/store/store.go:40`).
- **Metrics.** `/metrics` is plain text in Prometheus format, written by hand (`portal/internal/server/metrics.go:55`).
- **Tests.** `portal/internal/auth/*_test.go` cover hashing, session expiry, redirect validation, the rate limiter and TOTP. They run on every image build (`portal/Dockerfile`), so a failing test stops a deploy.
