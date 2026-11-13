# Notifications (ntfy)

Every service on this server can send a push notification to the iPhone and
to the Portal dashboard. The hub is [ntfy](https://docs.ntfy.sh/), running as
the `ntfy` container (`compose.notify.yml`, config in `ntfy/server.yml`).

```
 service ──POST──▶ ntfy (topic "backups") ──▶ iPhone app  (via Tailscale; ntfy.sh only relays a wake-up)
                         │                └─▶ Portal dashboard, Notifications panel (polls every 15 s)
 service ──POST──▶ Portal /api/notify ──(same token)──▶ ntfy
```

## Topics

A topic is just a name that publishers and subscribers agree on. There are six,
and nothing else is accepted (ntfy denies everything not listed in the access rules):

| Topic | What goes there | Sender (token) |
|---|---|---|
| `system` | Alerts about the machine and containers: app down, disk full, on battery, CPU hot | Alertmanager (`NTFY_TOKEN_ALERTS`) |
| `backups` | Backup failures (urgent) and the weekly summary (quiet) | backup container (`NTFY_TOKEN_BACKUP`) |
| `focal` | Messages from Focal (Pomodoro) | `NTFY_TOKEN_FOCAL` |
| `ritual` | Messages from Ritual (habits/to-dos) | `NTFY_TOKEN_RITUAL` |
| `dalal` | Messages from Dalal | `NTFY_TOKEN_DALAL` |
| `general` | Anything else: scripts, cron jobs, one-offs | `NTFY_TOKEN_GENERAL` |

## Priorities

| Priority | Name (`/api/notify`) | Use for | On the phone |
|---|---|---|---|
| 5 | `urgent` | Something is broken and needs you now | The most insistent alert the app can give |
| 3 | `normal` (default) | Worth knowing today | Normal notification |
| 1 | `quiet` | FYI, summaries | No sound, no vibration |

ntfy also understands 2 and 4; please stick to 5/3/1. On the dashboard urgent
messages get a red accent and quiet ones are dimmed.

## Tokens: where they live

All values are in `~/platform/.env` (mode 600, never committed). Names only:

| `.env` variable | ntfy user | May do |
|---|---|---|
| `NTFY_ADMIN_USER`, `NTFY_ADMIN_PASSWORD` | `kunchu01` (admin) | everything; use this in the iPhone app and at http://ntfy.exp.lab |
| `NTFY_TOKEN_PORTAL` | `svc-portal` | read the six topics, publish nothing |
| `NTFY_TOKEN_ALERTS` | `svc-alerts` | publish to `system` only |
| `NTFY_TOKEN_BACKUP` | `svc-backup` | publish to `backups` only |
| `NTFY_TOKEN_FOCAL` | `svc-focal` | publish to `focal` only |
| `NTFY_TOKEN_RITUAL` | `svc-ritual` | publish to `ritual` only |
| `NTFY_TOKEN_DALAL` | `svc-dalal` | publish to `dalal` only |
| `NTFY_TOKEN_GENERAL` | `svc-general` | publish to `general` only |

Service tokens are write-only: a leaked Focal token can post to `focal`, and
nothing else. It cannot read anything.

`scripts/ntfy-provision.sh` creates every missing value (never changes an existing
one), restarts ntfy if needed and checks that each token works. Run it any time.
Users, rules and tokens are declared in `compose.notify.yml`
(`NTFY_AUTH_USERS`, `NTFY_AUTH_ACCESS`, `NTFY_AUTH_TOKENS`); ntfy makes its
database match those lists on every start.

### Rotate a token

```sh
cd ~/platform
sed -i '/^NTFY_TOKEN_FOCAL=/d' .env      # forget the old one
scripts/ntfy-provision.sh                 # generates a new one, restarts ntfy
docker compose up -d portal               # only if you rotated NTFY_TOKEN_PORTAL
```

Then give the new value to the service.

### Add a token for a new service

Example: a service called `ledger` that should publish to `general`.

1. Add a variable to the `# --- Phase A: ntfy ---` section of `.env.example`
   (empty value) and to the token loop in `scripts/ntfy-provision.sh`
   (`TOKENS="... LEDGER"`).
2. In `compose.notify.yml` add one line to each list:
   ```yaml
   NTFY_AUTH_USERS:  ... ,svc-ledger:${NTFY_SVC_PASSWORD_HASH}:user
   NTFY_AUTH_ACCESS: ... ,svc-ledger:general:wo
   NTFY_AUTH_TOKENS: ... ,svc-ledger:${NTFY_TOKEN_LEDGER}:ledger
   ```
   (`wo` = write-only. A brand-new topic must also be added to `notify.Topics`
   in `portal/internal/notify/notify.go` for the dashboard and `/api/notify`.)
3. Run `scripts/ntfy-provision.sh`. It generates `NTFY_TOKEN_LEDGER`, restarts
   ntfy and checks the token.
4. Pass the value to the service, e.g. in its own `.env`:
   `grep ^NTFY_TOKEN_LEDGER= ~/platform/.env`.

Want a token by hand, outside the declared list? ntfy can generate one, and a
token made this way lives only in `data/ntfy/auth.db`:

```sh
docker compose exec ntfy ntfy token generate             # prints a random tk_... value
docker compose exec ntfy ntfy token list                 # shows all tokens (declared ones are marked)
```

## Sending a notification

Two equivalent ways. Both need a token.

**(a) Directly to ntfy.** Fewest moving parts. The message is the request body,
everything else is a header: `Authorization`, `Title`, `Priority` (1-5 or
`urgent`/`default`/`min`...), `Tags` (comma-separated), `Click` (URL).

**(b) Through Portal, `POST http://exp.lab/api/notify`.** A JSON API that checks
the request first (known topic, size, priority names) and returns clear errors.
Portal forwards your request **with your token**; it holds no write token
itself, so ntfy still decides what you may do. Limits: body 8 KB, message
4096 bytes, title 250 characters, 10 tags, 60 requests per minute per client.

```json
{"topic": "backups", "title": "Backup failed", "message": "restic exited with 1",
 "priority": "urgent", "tags": ["warning"], "click": "http://exp.lab/"}
```

`priority` is `"urgent"`, `"normal"`, `"quiet"` or 1-5 (default normal);
`title`, `tags` and `click` are optional. Answers:

| Status | Body | Meaning |
|---|---|---|
| 200 | `{"ok":true,"id":"..."}` | sent |
| 400 | `{"ok":false,"error":"..."}` | invalid request (the error says what) |
| 401 | `{"ok":false,"error":"..."}` | no/malformed token, or ntfy does not know it |
| 403 | `{"ok":false,"error":"this token may not publish to topic 'x'"}` | wrong topic for this token |
| 429 | | over 60/min (`Retry-After` says when to retry) |
| 502/503 | | ntfy is down or not configured |

### Which address to use

| Where the sender runs | ntfy directly | Portal helper |
|---|---|---|
| A device on the tailnet (laptop, phone) | `http://ntfy.exp.lab` | `http://exp.lab/api/notify` |
| A process on this server (cron, systemd, scripts) | `http://127.0.0.1:2586` | `http://127.0.0.1:7080/api/notify` |
| A container that joins the `platform_internal` network | `http://ntfy:80` | (use ntfy directly) |
| Any other container (its own bridge network) | `http://ntfy.exp.lab` with `extra_hosts` below | `http://exp.lab/api/notify` with `extra_hosts` |

A container on its own bridge network cannot resolve `*.exp.lab` (Docker's DNS
does not know the private zone), so map the names to the Tailscale IP where
Caddy listens:

```yaml
services:
  myapp:
    extra_hosts:
      - "ntfy.exp.lab:100.118.33.89"
      - "exp.lab:100.118.33.89"
```

Or join the shared network and talk to ntfy by container name:

```yaml
services:
  myapp:
    networks: [default, platform_internal]
networks:
  platform_internal:
    external: true        # created by the platform project
```

### curl

```sh
TOKEN=$(sed -n 's/^NTFY_TOKEN_GENERAL=//p' ~/platform/.env)

# (a) direct to ntfy
curl -H "Authorization: Bearer $TOKEN" \
     -H "Title: Disk check" -H "Priority: 3" -H "Tags: floppy_disk" \
     -d "Disk is at 61%" \
     http://ntfy.exp.lab/general

# (b) through Portal
curl -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
     -d '{"topic":"general","title":"Disk check","message":"Disk is at 61%","priority":"normal","tags":["floppy_disk"]}' \
     http://exp.lab/api/notify
```

On the server itself, replace `http://ntfy.exp.lab` with `http://127.0.0.1:2586`.

### Python (standard library only)

```python
import json
import os
import urllib.request

NTFY = os.environ.get("NTFY_URL", "http://ntfy.exp.lab")
PORTAL = os.environ.get("PORTAL_URL", "http://exp.lab")
TOKEN = os.environ["NTFY_TOKEN"]  # e.g. the value of NTFY_TOKEN_GENERAL


def notify_direct(topic, message, title="", priority=3, tags=()):
    """(a) POST the text to ntfy; metadata goes in headers."""
    headers = {"Authorization": f"Bearer {TOKEN}", "Priority": str(priority)}
    if title:
        headers["Title"] = title  # HTTP headers must be Latin-1: keep titles ASCII here
    if tags:
        headers["Tags"] = ",".join(tags)
    req = urllib.request.Request(f"{NTFY}/{topic}", data=message.encode(), headers=headers)
    with urllib.request.urlopen(req, timeout=10) as resp:
        return json.load(resp)["id"]


def notify_portal(topic, message, title="", priority="normal", tags=()):
    """(b) POST JSON to Portal's helper, which forwards it with our token."""
    body = json.dumps({"topic": topic, "title": title, "message": message,
                       "priority": priority, "tags": list(tags)}).encode()
    req = urllib.request.Request(f"{PORTAL}/api/notify", data=body, headers={
        "Authorization": f"Bearer {TOKEN}", "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            return json.load(resp)["id"]
    except urllib.error.HTTPError as e:  # 4xx/5xx carry {"ok":false,"error":"..."}
        raise RuntimeError(f"notify failed ({e.code}): {json.load(e)['error']}") from None


if __name__ == "__main__":
    print(notify_direct("general", "Hello from Python (direct)", title="Python", tags=["snake"]))
    print(notify_portal("general", "Hello from Python (Portal)", title="Python", priority="quiet"))
```

Run it: `NTFY_TOKEN=$(sed -n 's/^NTFY_TOKEN_GENERAL=//p' ~/platform/.env) python3 notify.py`.
With `requests` installed the helper is one line:
`requests.post(f"{PORTAL}/api/notify", json={...}, headers={"Authorization": f"Bearer {TOKEN}"}, timeout=10)`.

### Go (standard library only)

```go
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

var client = &http.Client{Timeout: 10 * time.Second}

// notifyDirect: (a) the text is the body, metadata goes in headers.
func notifyDirect(base, token, topic, title, message string, priority int, tags ...string) error {
	req, _ := http.NewRequest("POST", base+"/"+topic, strings.NewReader(message))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Title", title)
	req.Header.Set("Priority", fmt.Sprint(priority))
	req.Header.Set("Tags", strings.Join(tags, ","))
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ntfy: HTTP %d", resp.StatusCode)
	}
	return nil
}

// notifyPortal: (b) JSON to Portal's helper, which forwards it with our token.
func notifyPortal(base, token, topic, title, message, priority string, tags ...string) (string, error) {
	body, _ := json.Marshal(map[string]any{"topic": topic, "title": title,
		"message": message, "priority": priority, "tags": tags})
	req, _ := http.NewRequest("POST", base+"/api/notify", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		OK    bool   `json:"ok"`
		ID    string `json:"id"`
		Error string `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if !out.OK {
		return "", fmt.Errorf("notify: HTTP %d: %s", resp.StatusCode, out.Error)
	}
	return out.ID, nil
}

func main() {
	token := os.Getenv("NTFY_TOKEN")
	if err := notifyDirect("http://ntfy.exp.lab", token, "general", "Go", "Hello from Go (direct)", 3, "rocket"); err != nil {
		fmt.Println("direct:", err)
	}
	id, err := notifyPortal("http://exp.lab", token, "general", "Go", "Hello from Go (Portal)", "quiet")
	fmt.Println("portal:", id, err)
}
```

## Reading notifications

- **iPhone**: the ntfy app, see the README section [iPhone ntfy app setup](README.md#iphone-ntfy-app-setup).
- **Dashboard**: the Notifications panel on http://exp.lab shows the last 20
  messages across all six topics (Portal polls ntfy every 15 s with the
  read-only `NTFY_TOKEN_PORTAL`, and immediately after each `/api/notify`).
- **Web**: http://ntfy.exp.lab, log in with `NTFY_ADMIN_USER` / `NTFY_ADMIN_PASSWORD`.
- **Command line**:
  ```sh
  TOKEN=$(sed -n 's/^NTFY_TOKEN_PORTAL=//p' ~/platform/.env)
  curl -s -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:2586/system,backups,focal,ritual,dalal,general/json?poll=1&since=1h"
  curl -sN -H "Authorization: Bearer $TOKEN" http://127.0.0.1:2586/system/json   # live stream, Ctrl-C to stop
  ```

Messages are kept for 7 days (`cache-duration` in `ntfy/server.yml`).

## Privacy: what leaves the server

For iPhone push, ntfy sends ntfy.sh a "poll request" per message: the message
ID and a SHA-256 hash of the topic URL, nothing else. Apple wakes the app, and
the app fetches the actual message from `ntfy.exp.lab` over Tailscale. Title
and text never leave your network. Details: comments in `ntfy/server.yml`.
