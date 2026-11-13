# Backups and restores

Every night at 03:00 IST the `backup` container takes an encrypted,
deduplicated snapshot of every app's data with [restic](https://restic.net/).
This file explains what is in a snapshot, how to get things back (one file,
one app, or the whole machine) and how to move the backups to Google Drive.

**The one thing you must not lose: `RESTIC_PASSWORD` in `~/platform/.env`.**
Every snapshot is encrypted with it. Without it nobody, including you, can read
the backups. Put a copy in your password manager now, outside this machine.

Contents:
1. [What is backed up](#1-what-is-backed-up)
2. [Concepts in five minutes](#2-concepts-in-five-minutes)
3. [Everyday commands](#3-everyday-commands)
4. [Restore to staging](#4-restore-to-staging-the-safe-first-step)
5. [Restore one file](#5-restore-one-file)
6. [Restore one app](#6-restore-one-app)
7. [Disaster recovery: a new machine](#7-disaster-recovery-a-new-machine)
8. [Google Drive (off-site copy)](#8-google-drive-off-site-copy)
9. [What NOT to do](#9-what-not-to-do)
10. [When something goes wrong](#10-when-something-goes-wrong)

---

## 1. What is backed up

| App | On the server | How |
|---|---|---|
| Portal | `~/platform/data` (`portal.db`, `apps.yaml`, `ntfy/*.db`, alertmanager state) | SQLite copy + files |
| Focal | `~/focal/data` (`focal.db`) | SQLite copy + files |
| Ritual | `~/apps/ritual/data` (`ritual.db`) | SQLite copy + files |
| Ledger | `~/apps/ledger/data` (`ledger.db`, `secret.key`) | SQLite copy + files |
| Gitea | `~/gitea/data` (`gitea.db`, repositories, LFS, `app.ini`, JWT key, avatars, attachments, ssh host keys) | SQLite copy + files |
| DSA tracker | `~/dsa-tracker/dsa_tracker.db` | SQLite copy only |
| Ramayana IMAX bot | `~/ramayana-imax-bot/state.db` | SQLite copy only |
| Dalal | Postgres `dalal` in `dalal-postgres-1` | `pg_dump` (plain SQL) |
| Conveyor | Postgres `conveyor` in `conveyor-postgres-1` | `pg_dump` (plain SQL) |

**Not** backed up on purpose: metrics (`data/vm`), caches, logs, `node_modules`,
Gitea's `sessions/ log/ tmp/ indexers/ queues/ repo-archive/` (Gitea rebuilds
them), the apps' own `backups/` folders, and **source code** (it lives in git;
push your repositories somewhere off this machine). Full rules:
`backup/excludes.txt`; the list of databases: `SQLITE_DBS` and `PG_DBS` in
`backup/scripts/lib.sh`.

### Layout inside a snapshot

```
/staging/MANIFEST.txt                 what this snapshot is, tool versions, sizes
/staging/sqlite/platform/portal.db    consistent copies of every SQLite database
/staging/sqlite/platform/ntfy/auth.db   (same relative path as under /src)
/staging/sqlite/gitea/gitea/gitea.db
/staging/sqlite/...
/staging/postgres/dalal.sql           pg_dump plain SQL
/staging/postgres/conveyor.sql
/src/platform/...   = ~/platform/data      (WITHOUT the live .db files)
/src/focal/...      = ~/focal/data
/src/ritual/...     = ~/apps/ritual/data
/src/ledger/...     = ~/apps/ledger/data   (secret.key is here)
/src/gitea/...      = ~/gitea/data         (git/, gitea/conf, gitea/jwt, ssh/ ...)
```

Rule of thumb: **databases come from `/staging`, everything else from `/src`.**

### Schedule (IST)

| When | What | Script |
|---|---|---|
| daily 03:00 | backup, then keep 7 daily / 4 weekly / 6 monthly snapshots and prune | `backup.sh nightly` |
| Sunday 04:00 | `restic check` + read 1/10 of the data (all of it every 10 weeks) | `check.sh` |
| 1st of month 04:30 | restore the newest snapshot to a scratch dir and verify it | `restore-test.sh` |
| Sunday 09:00 | quiet weekly summary to ntfy topic `backups` | `weekly-summary.sh` |

Failures of any of these send an **urgent** ntfy message (topic `backups`).
A successful backup is silent on purpose (a daily "all good" teaches you to
ignore the topic); the weekly summary and the BackupStale alert cover that.

---

## 2. Concepts in five minutes

**3-2-1.** Keep 3 copies of your data, on 2 different kinds of storage, 1 of
them off-site. Today you have the live data plus the local repository
`/var/backups/restic-local`, **on the same disk**. That protects against
mistakes (deleted a file, broken upgrade, corrupt database) but not against
the disk dying, theft or fire: for those it counts as nothing. Google Drive
(section 8) is the off-site copy. (`select_repo` in `backup/scripts/lib.sh`.)

**Incremental and deduplicated.** restic cuts every file into chunks at
positions chosen by the content itself ("content-defined chunking"), names
each chunk by its hash, and stores each chunk once. Tonight's backup only adds
the chunks that did not exist yet; unchanged files cost nothing, and a 25 MB
SQL dump in which a few rows changed costs a few hundred KB. Every snapshot is
still a complete picture you can restore on its own; there are no "full" and
"incremental" sets to chain together. That is also why the Postgres dumps are
plain, uncompressed SQL: compressing them would change every byte when one row
changes and defeat deduplication (restic compresses the repository itself).
(`stage_postgres` and the `restic backup` step in `backup/scripts/backup.sh`.)

**Why a live SQLite database cannot simply be copied.** The app keeps writing
into the file while `cp` reads it: the copy can contain half of a write (a
"torn page") and be corrupt. In WAL mode it is worse: recent changes are not in
`app.db` at all but in `app.db-wal` until a checkpoint, so `app.db` alone is
old and `app.db` + `app.db-wal` copied at slightly different moments do not
match. `sqlite3 app.db ".backup copy.db"` instead reads the database through
SQLite, under a read lock, and writes a consistent, self-contained copy (WAL
content included). The backup runs it as the user that owns the database, so
it never leaves root-owned `-wal`/`-shm` files next to a live database, and
checks every copy with `PRAGMA integrity_check`.
(`stage_sqlite_db` in `backup/scripts/backup.sh`.)

**Why restore tests matter.** A backup is only worth what you can restore from
it. Wrong password, a repository with missing pieces, a dump that stops halfway,
a folder that was excluded by mistake: none of this shows until you restore.
So on the 1st of every month the newest snapshot is restored for real and every
database in it is opened and checked (`backup/scripts/restore-test.sh`), and
`restic check` verifies the repository weekly (`backup/scripts/check.sh`).

**Dead man's switch.** A backup that fails sends a message. A backup that
silently stops running (container gone, cron broken) sends nothing. So the
backup writes `backup_last_success_timestamp_seconds` to
`run/textfile/backup.prom`; monitoring raises **BackupStale** when that time is
more than 26 hours old, or when the metric is missing altogether.
(`write_metrics` in `backup/scripts/lib.sh`.)

---

## 3. Everyday commands

All commands run in `~/platform`. `restic.sh` is a small wrapper that points
restic at the current repository (local or Drive) with the password set.

```sh
docker compose exec backup /scripts/backup.sh manual          # back up now
docker compose exec backup /scripts/restic.sh snapshots       # list snapshots
docker compose exec backup /scripts/restic.sh ls latest /staging          # browse
docker compose exec backup /scripts/restic.sh ls latest /src/gitea/gitea/conf
docker compose exec backup /scripts/restic.sh find 'apps.yaml'            # which snapshots have it
docker compose exec backup /scripts/restic.sh diff 1a2b3c4d 5e6f7a8b      # what changed between two
docker compose exec backup /scripts/restic.sh stats --mode raw-data       # repository size
docker compose exec backup cat /work/logs/backup.log          # log of the last backup
docker compose logs --tail 100 backup                         # everything the container said
cat data/backup/status.json | jq                              # what Portal shows
cat run/textfile/backup.prom                                  # the metrics
```

`latest` means the newest snapshot; any snapshot can be named by the first 8
characters of its ID (from `snapshots`).

---

## 4. Restore to staging (the safe first step)

Restoring to staging never touches live data. It unpacks a whole snapshot into
`~/restore-staging/<UTC date-time>-<snapshot id>/`, owned by you (mode 700:
it contains secrets), and checks every SQLite file in it.

- **Dashboard**: Backups panel, "Restore to staging" (newest snapshot).
- **Command line**:
  ```sh
  docker compose exec backup /scripts/restore-staging.sh latest
  docker compose exec backup /scripts/restore-staging.sh 1a2b3c4d   # a specific snapshot
  ```

The result (path, how many SQLite files passed `integrity_check`) is in
`data/backup/status.json` (`last_staging_restore`) and in the log. Only the
newest 3 staging restores are kept; older ones are deleted automatically
(only directories with that exact naming pattern). Delete them yourself when
you are done: `rm -rf ~/restore-staging/2026*`.

The script refuses any argument other than `latest` or 8-64 hex characters,
and builds the target path itself; it cannot write anywhere else.

---

## 5. Restore one file

Small text file, straight out of the newest snapshot:

```sh
cd ~/platform
docker compose exec -T backup /scripts/restic.sh dump latest /src/platform/apps.yaml > /tmp/apps.yaml
diff /tmp/apps.yaml data/apps.yaml        # look before you overwrite anything
```

Anything else (a database, a folder): restore to staging (section 4), then
copy what you need from `~/restore-staging/<dir>/...`. For a database always
take the copy under `staging/sqlite/`, and follow section 6 (stop the app first).

---

## 6. Restore one app

General recipe, for every app:

1. **Restore to staging** (section 4). Let `R` be that directory:
   `R=$(ls -d ~/restore-staging/2* | tail -1)`.
2. **Stop the app.** Never replace a database under a running app.
3. **Move the current data aside, never delete it**:
   `mv app.db app.db.before-restore-$(date +%F)` (and its `-wal`/`-shm` files).
4. **Copy** the restored files in, keeping the owner (you, uid 1000) and modes.
5. **Start** the app and **check** it (log in, look at recent data).
6. When you are sure, delete the `.before-restore` files.

Per app (each block starts in the app's folder):

```sh
# Portal + ntfy (~/platform)
docker compose stop portal ntfy
mkdir -p data/before-restore && mv data/portal.db* data/ntfy/*.db* data/before-restore/
install -m 600 "$R/staging/sqlite/platform/portal.db" data/portal.db
install -m 644 "$R/staging/sqlite/platform/ntfy/cache.db" "$R/staging/sqlite/platform/ntfy/auth.db" data/ntfy/
cp "$R/src/platform/apps.yaml" data/apps.yaml          # only if it changed
docker compose up -d portal ntfy

# Focal (~/focal)
docker compose stop
mv data/focal.db data/focal.db.before-restore-$(date +%F)
install -m 644 "$R/staging/sqlite/focal/focal.db" data/focal.db
docker compose start

# Ritual (~/apps/ritual, a user systemd service)
systemctl --user stop ritual
for f in data/ritual.db*; do mv "$f" "$f.before-restore-$(date +%F)"; done
install -m 644 "$R/staging/sqlite/ritual/ritual.db" data/ritual.db
systemctl --user start ritual

# Ledger (~/apps/ledger) - the database AND secret.key belong together
docker compose stop
for f in data/ledger.db* data/secret.key; do mv "$f" "$f.before-restore-$(date +%F)"; done
install -m 644 "$R/staging/sqlite/ledger/ledger.db" data/ledger.db
install -m 600 "$R/src/ledger/secret.key" data/secret.key
docker compose start

# DSA tracker (~/dsa-tracker) and the Ramayana bot (~/ramayana-imax-bot)
systemctl --user stop dsa-tracker            # bot: systemctl --user stop ramayana-imax-bot.timer
mv dsa_tracker.db dsa_tracker.db.before-restore-$(date +%F)
install -m 644 "$R/staging/sqlite/dsa-tracker/dsa_tracker.db" dsa_tracker.db
systemctl --user start dsa-tracker           # bot: state.db from staging/sqlite/ramayana-imax-bot/
```

**Gitea** (`~/gitea`; data is mounted at `/data` in the container, runs as uid 1000):

```sh
cd ~/gitea && docker compose stop
D=~/gitea/data
mv $D/gitea/gitea.db $D/gitea/gitea.db.before-restore-$(date +%F)
install -m 644 "$R/staging/sqlite/gitea/gitea/gitea.db" $D/gitea/gitea.db
# repositories, LFS, conf, keys, avatars, attachments: copy over (keep the old ones aside first)
mv $D/git $D/git.before-restore-$(date +%F) && cp -a "$R/src/gitea/git" $D/git
cp -a "$R/src/gitea/gitea/." $D/gitea/                  # conf/app.ini, jwt/, avatars/, attachments/ ...
# ssh host keys are root-owned on the server (only needed if they were lost):
#   sudo install -o root -g root -m 600 "$R"/src/gitea/ssh/ssh_host_*_key     $D/ssh/
#   sudo install -o root -g root -m 644 "$R"/src/gitea/ssh/ssh_host_*_key.pub $D/ssh/
docker compose start
docker exec gitea-gitea-1 gitea admin regenerate hooks    # if pushes complain about hooks
```

**Postgres** (Dalal: `~/apps/dalal`, services `api worker`; Conveyor:
`~/conveyor`, service `api`). Load the dump into a *fresh* database; keep the
old one under another name until you are sure:

```sh
cd ~/apps/dalal
docker compose stop api worker                       # nothing may be connected
docker exec dalal-postgres-1 psql -U dalal -d postgres -c 'ALTER DATABASE dalal RENAME TO dalal_before_restore'
docker exec dalal-postgres-1 psql -U dalal -d postgres -c 'CREATE DATABASE dalal OWNER dalal'
docker exec -i dalal-postgres-1 psql -U dalal -d dalal -v ON_ERROR_STOP=1 < "$R/staging/postgres/dalal.sql"
docker compose start api worker
# later, when everything looks right:
# docker exec dalal-postgres-1 psql -U dalal -d postgres -c 'DROP DATABASE dalal_before_restore'
```

(Same for conveyor with `conveyor` everywhere.) The dumps contain `\restrict`
lines, which need psql 17.6 or newer: use the psql inside the postgres:17
container as above, not an old psql on the host.

---

## 7. Disaster recovery: a new machine

The disk died, or the machine is gone. The local repository died with it; you
restore from **Google Drive** (section 8). You need two things that are not on
the machine: the **`RESTIC_PASSWORD`** (password manager) and access to the
Google account.

1. **Install** Ubuntu with a user `kunchu01` (uid 1000), then Docker
   (docs.docker.com/engine/install/ubuntu) and git; add yourself to the
   `docker` group; log out and in.
2. **Get the code**: `git clone https://github.com/Variiish06/platform.git ~/platform`
   (and the app repositories into their old places).
3. **Recreate `.env`**: `cp .env.example .env && chmod 600 .env`, fill in the
   values (section by section, the comments explain them). Put the **old
   `RESTIC_PASSWORD`** back, and `BACKUP_TARGET=drive`.
4. **Google Drive access**: repeat section 8, steps 1-3 (the authorisation
   with the same Google account sees the existing repository, because
   rclone created it).
5. **Start only the backup container** and check you can see your snapshots:
   ```sh
   mkdir -p ~/restore-staging data/backup/requests run/portal run/textfile secrets
   chmod 700 secrets && [ -e secrets/rclone.conf ] || install -m 600 /dev/null secrets/rclone.conf   # must exist before the first start
   docker compose up -d --build backup docker-proxy-backup
   backup/drive-setup.sh            # paste the token from step 4 (this is section 8, step 4)
   docker compose exec backup /scripts/restic.sh snapshots
   ```
   "wrong password" here means the password in `.env` is not the old one.
6. **Restore to staging**: `docker compose exec backup /scripts/restore-staging.sh latest`.
   `R=$(ls -d ~/restore-staging/2* | tail -1)`; read `$R/staging/MANIFEST.txt`.
7. **Put the data in place before starting each app**, one app at a time,
   following section 6 (on a fresh machine there is no old data to move aside):
   - `~/platform/data`: copy `$R/src/platform/.` there, then the databases from
     `$R/staging/sqlite/platform/` (portal.db mode 600). Then
     `docker compose up -d` the platform services.
   - Focal, Ritual, Ledger, DSA tracker, the bot: copy their `/src/<app>` files,
     then their databases from `$R/staging/sqlite/<app>/`, then start them.
   - Gitea: `sudo mkdir -p ~/gitea/data/ssh`, copy `$R/src/gitea/git` and
     `$R/src/gitea/gitea` into `~/gitea/data/`, the database from staging, the
     ssh keys with `sudo install` as in section 6, `sudo chown root:root ~/gitea/data`
     (as it was), start it.
   - Dalal, Conveyor: start only their `postgres` service (it creates an empty
     database), load the dump with `psql` as in section 6 (no rename needed),
     then start the rest.
8. **Verify** every app in the browser, then run a backup
   (`docker compose exec backup /scripts/backup.sh manual`) and delete
   `~/restore-staging/*`.

No platform repository at hand? restic itself is enough. On any computer with
restic and rclone, the same rclone config and the password:
`restic -r rclone:gdrive:experimentos-restic snapshots`, then
`restic -r ... restore latest --target ./restored`.

---

## 8. Google Drive (off-site copy)

The backup is ready for Drive; it only needs your authorisation, which must
happen in a browser. About 10 minutes.

**On the Windows PC** (PowerShell):

1. Install rclone: `winget install Rclone.Rclone`, then open a **new**
   PowerShell window (alternatives: `scoop install rclone`, or the zip from
   rclone.org/downloads; unzip and run `.\rclone.exe` from that folder).
2. Run exactly:
   ```powershell
   rclone authorize "drive" "eyJzY29wZSI6ImRyaXZlLmZpbGUifQ"
   ```
   A browser opens: sign in with your Google account and allow access. (The
   second argument is base64 for `{"scope":"drive.file"}`: rclone may only see
   files it created itself, not the rest of your Drive. restic only needs its
   own files, so this is the narrowest permission that works.)
3. PowerShell prints a block between `Paste the following into your remote
   machine --->` and `<---End paste`. Copy that one line, which starts with
   `{"access_token":`.

**On the server** (`ssh` in, then):

4. Run the helper and paste the line when asked:
   ```sh
   cd ~/platform && backup/drive-setup.sh
   ```
   It writes `secrets/rclone.conf` (mode 600, remote `gdrive`) and tests it
   with `rclone lsd gdrive:` inside the backup container (an empty listing is
   normal).
5. Switch the backups to Drive and take the first one (it creates the
   repository `experimentos-restic` in your Drive automatically):
   ```sh
   sed -i 's/^BACKUP_TARGET=.*/BACKUP_TARGET=drive/' .env
   docker compose up -d backup
   docker compose exec backup /scripts/backup.sh manual
   ```
6. Check: `docker compose exec backup /scripts/restic.sh snapshots`, and in the
   Drive web UI a folder `experimentos-restic` appears.

Going back to the local repository: the same `sed` with `local`, then
`docker compose up -d backup`. The two repositories are independent: each has
its own snapshots, and the local one simply stops receiving new ones while
Drive is selected.

Notes: rclone refreshes the Google token by itself (hourly) and saves it in
the container's work volume (`/work/rclone/rclone.conf`); `secrets/rclone.conf`
stays as you wrote it. If Google ever revokes the token (password change,
"remove access"), backups fail with an urgent message: run steps 2-4 again.

---

## 9. What NOT to do

- **Never restore over live data while the app runs.** Stop it first (section 6).
- **Never copy a `-wal` file without its database**, or a database without its
  `-wal` from the same moment. Use the `staging/sqlite/` copies: they are
  single, complete files. Delete or move any old `-wal`/`-shm` next to the
  target before putting a restored database in place.
- **Never delete the old data** before the restored app is verified: move it aside.
- **Never change or lose `RESTIC_PASSWORD`.** Changing it in `.env` does not
  re-encrypt anything; it just locks you out (use `restic key add/passwd` if
  you ever must change it).
- Do not run `restic forget`, `prune`, `key remove` or `unlock --remove-all`
  by hand unless you understand them; the nightly job does the pruning.
- Do not put files of your own into `~/restore-staging/` with names like
  `20261112-194757-9a210b74`: the cleanup would remove them.
- Do not run `docker compose down` (it stops every platform service); use
  `docker compose stop backup` / `up -d backup` for this one.

---

## 10. When something goes wrong

| Symptom | Look at | Usual cause |
|---|---|---|
| "Backup FAILED" on the phone | the message text, then `docker compose exec backup cat /work/logs/backup.log` | an app's database moved/renamed (update `SQLITE_DBS` in `backup/scripts/lib.sh`), a Postgres container down, disk full, Drive token revoked |
| "wrong password" (exit 12) | `.env` | `RESTIC_PASSWORD` changed |
| BackupStale alert, no failure message | `docker compose ps backup`, `docker compose logs backup` | container not running |
| "repository is already locked" | `docker compose exec backup /scripts/restic.sh list locks` | a run was killed; the next run removes stale locks itself |
| "database not in SQLITE_DBS" warning in the log | the path in the warning | a new database appeared; it was copied anyway; add it to the list |

Files: `compose.backup.yml` (container, mounts, capabilities),
`backup/Dockerfile`, `backup/crontab`, `backup/excludes.txt`,
`backup/scripts/*.sh`, `backup/drive-setup.sh`. Status for Portal:
`data/backup/status.json`; requests from Portal: `data/backup/requests/`.
