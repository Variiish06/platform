# Platform: the front door of `experimentos`

One small stack that gives every app on this server a clean name, one login and one dashboard.

| URL | What | Portal login |
|---|---|---|
| `http://exp.lab/` | Dashboard (apps, system, containers) | yes |
| `http://focal.exp.lab/` | Focal (pomodoro) | yes |
| `http://ritual.exp.lab/` | Ritual (habits, to-dos) | yes |
| `http://bot.exp.lab/` | Groq bots UI | yes |
| `http://git.exp.lab/` | Gitea | no (Gitea's own login) |
| `http://exp.lab/healthz` | Portal health | no |
| `http://exp.lab/metrics` | Prometheus metrics | yes (or direct on `127.0.0.1:7080`) |

Everything is reachable over Tailscale (and from the LAN). Nothing listens on the public internet.

## Architecture

```
 iPhone / Windows PC (Tailscale on)
   │
   │ 1. "where is focal.exp.lab?"   Tailscale Split DNS sends *.exp.lab lookups to
   ▼                                100.118.33.89:53
 ┌──────────────┐
 │   CoreDNS    │  answers every *.exp.lab name with 100.118.33.89
 └──────────────┘
   │
   │ 2. GET http://focal.exp.lab/            (inside the Tailscale tunnel)
   ▼
 ┌──────────────┐   3. forward_auth: "may this request pass, and where does
 │    Caddy     │      focal.exp.lab live?"      ┌───────────────────────────┐
 │  :80 on the  │ ─────────────────────────────▶ │  Portal  127.0.0.1:7080   │
 │ Tailscale IP │ ◀───────────────────────────── │  login, sessions (SQLite) │
 │  and LAN IP  │   200 + X-Portal-Upstream      │  dashboard, /metrics      │
 └──────────────┘   or 302 to exp.lab/login      │  reads data/apps.yaml     │
   │                                             └───────────────────────────┘
   │ 4. proxy to the upstream Portal named            │ unix socket (./run)
   ▼                                                  ▼
  Focal 127.0.0.1:8787    Ritual 127.0.0.1:8090    ┌────────────────────────┐
  Gitea 127.0.0.1:3000    Bots   127.0.0.1:8080    │  docker-socket-proxy   │
                                                   │  list / stats / restart│
                                                   └────────────────────────┘
                                                      │ /var/run/docker.sock
                                                      ▼  Docker
```

Four containers, defined in `docker-compose.yml`:

| Container | Image | Job | Memory limit |
|---|---|---|---|
| `caddy` | `caddy:2-alpine` | Reverse proxy on port 80 | 64 MB |
| `coredns` | `coredns/coredns` | DNS for `*.exp.lab` | 32 MB |
| `portal` | built from `./portal` (Go) | Auth + dashboard | 96 MB |
| `docker-proxy` | `wollomatic/socket-proxy` | Filters Docker API calls | 24 MB |

Files:

```
docker-compose.yml     the whole stack
.env                   machine settings (gitignored; see .env.example)
caddy/Caddyfile        proxy rules (does not list apps)
caddy/entrypoint.sh    picks the addresses Caddy listens on
coredns/Corefile       the exp.lab zone
data/apps.yaml         the list of apps  <- the file you edit
data/portal.db         SQLite: password hash, sessions (gitignored)  <- back up data/
portal/                Go source of Portal
portalctl              runs Portal admin commands
```

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

## How to add a new app

Add one entry to `data/apps.yaml`:

```yaml
  - id: ledger                 # becomes http://ledger.exp.lab/
    name: Ledger
    description: Expenses
    icon: wallet
    upstream: 127.0.0.1:8095   # where the app really listens
```

Save. Within 30 seconds the app is routed, behind the login, on the dashboard and health-checked. Nothing needs a restart, and no DNS or Caddy change is needed.

- `auth: false` skips the Portal login (for apps with their own, like Gitea).
- `health: /some/path` changes the path the health check requests.
- The app must be reachable from the host. An app in Docker needs a published port, for example `127.0.0.1:8095:8095`.
- If the file has a mistake, the old version stays active and the dashboard shows the error.

## How to change your password

```bash
cd ~/platform
./portalctl set-password
```

This also signs out every existing session. Other commands:

```bash
./portalctl sessions            # who is signed in
./portalctl sessions revoke     # sign out everywhere
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

## Pushing this repo to Gitea

The work is committed locally, one commit per phase. To put it in Gitea:

1. In Gitea, click **+** → **New Repository**, name it `platform`, leave "Initialize repository" unticked, and create it.
2. Then:

```bash
cd ~/platform
git remote add origin http://localhost:3000/kunchu47/platform.git
git push -u origin main      # asks for your Gitea username and password once
```

## Day-to-day commands

```bash
cd ~/platform
docker compose ps                    # status
docker compose logs -f portal        # logins, restarts, config errors
docker compose up -d --build portal  # rebuild Portal after changing its code (runs the tests)
docker stats --no-stream $(docker compose ps -q)   # memory per container
portal/gorun.sh go test ./...        # run the Go tests (Go runs in a container)
```

## Adding HTTPS later

Plain HTTP is acceptable today because every byte travels inside Tailscale's encrypted tunnel. HTTPS becomes worth it if you want the browser padlock, Home Screen web apps with service workers or push, or LAN access without Tailscale.

Caddy can be its own certificate authority (CA):

1. In `caddy/Caddyfile`: delete `auto_https off`, change the three site addresses from `http://…` to `https://…`, and add `tls internal` inside the `{$DOMAIN}` and `*.{$DOMAIN}` blocks.
2. In `docker-compose.yml`, give Caddy a volume so its CA survives restarts: `- ./caddy/data:/data`.
3. Port 443 on the Tailscale IP is currently taken by `tailscale serve`. Check with `tailscale serve status` and turn it off (`tailscale serve --https=443 off`) or Caddy cannot bind.
4. `docker compose up -d caddy`, then copy the CA certificate out: `docker compose cp caddy:/data/caddy/pki/authorities/local/root.crt .`
5. Trust `root.crt` on each device:
   - iPhone: send yourself the file, install the profile, then Settings → General → About → Certificate Trust Settings → turn it on.
   - Windows: double-click → Install Certificate → Local Machine → Trusted Root Certification Authorities.
6. Set `COOKIE_SECURE=1` in `.env` and `docker compose up -d portal`. The cookie then gets the `Secure` flag and Portal builds `https://` links. You will need to sign in again.

## Troubleshooting

| Symptom | Likely cause and fix |
|---|---|
| Browser searches for "exp.lab" or says site can't be reached | Type `http://exp.lab/` in full. Then check DNS: `nslookup exp.lab` must answer `100.118.33.89`. If it does not, redo the Split DNS steps and check "Use Tailscale DNS settings" on the device |
| DNS answers but the page does not load | Is Tailscale connected on the device? On the server: `docker compose ps` and `docker compose logs caddy` |
| Safari shows a certificate warning | It tried `https://`. Use `http://exp.lab/` |
| Login says "No password has been set yet" | Run `./portalctl set-password` |
| "Too many attempts" | Wait for the time shown. It doubles with each further failure (2s, 4s, 8s … 15 min) and resets after a correct login. Restarting Portal also clears it: `docker compose restart portal` |
| Signed in, but an app still sends you to the login page | The cookie did not reach the subdomain. Sign in at `http://exp.lab/` (not at an IP address), and check the browser is not blocking cookies |
| Tile shows "Down · connection refused" | The app is not running. Find it under Containers and press its start/restart button |
| `502` from an app URL | Same as above: Caddy could not connect to the app's port |
| `404 No app is configured for …` | That hostname has no entry in `data/apps.yaml` |
| Dashboard says "Can't reach Docker" | `docker compose up -d docker-proxy` |
| New app does not appear | Wait 30 seconds. Then `docker compose logs portal` for an `apps.yaml` error |
| `git clone http://git.exp.lab/...` asks for a login | Expected: that is Gitea's own login |
| After a reboot nothing answers for a few seconds | Caddy and CoreDNS wait for Tailscale to come up and retry by themselves |
| LAN IP changed (DHCP) and LAN access stopped | `docker compose restart caddy`. Tailscale access is not affected |
| Locked out completely | The old direct ports still work, and SSH is untouched. `cd ~/platform && docker compose logs` |

To stop the whole stack: `docker compose down`. The apps keep running on their old ports.
