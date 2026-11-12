# Backup container: restic (encrypted, deduplicated backups) + the tools it
# needs to collect consistent copies of every database first.
# Built by compose.backup.yml; scripts and how it all fits: RESTORE.md.
#
# Pinned base image: change the tag on purpose and rebuild
# ("docker compose build backup"). Alpine 3.24 ships restic 0.18 and
# rclone 1.75 (the exact versions are printed in every backup log).
FROM alpine:3.24.4

# restic   - the backup program (encryption, dedup, snapshots)
# rclone   - lets restic talk to Google Drive ("rclone:" repositories)
# sqlite   - sqlite3 CLI: ".backup" = consistent copy of a live database
# docker-cli - "docker exec ... pg_dump" through the backup socket proxy
# tini     - PID 1: forwards signals, reaps zombie processes
# bash, jq, curl, tar (GNU), tzdata, setpriv (drop to another uid), flock
# (one job at a time), util-linux-misc (mountpoint)
RUN apk add --no-cache \
        restic rclone sqlite docker-cli tini \
        bash jq curl tar tzdata setpriv flock util-linux-misc ca-certificates

# supercronic: a cron made for containers (logs to stdout, honours TZ,
# never runs a job twice at the same time). Release binary pinned by
# version AND checksum: if GitHub ever served different bytes, the build fails.
ARG SUPERCRONIC_VERSION=v0.2.51
ARG SUPERCRONIC_SHA256=cc232108374e8e2b537045f2d5eaa1229f619db314c3ba6a14d38b35dc0aa123
RUN curl -fsSL -o /usr/local/bin/supercronic \
        "https://github.com/aptible/supercronic/releases/download/${SUPERCRONIC_VERSION}/supercronic-linux-amd64" \
 && echo "${SUPERCRONIC_SHA256}  /usr/local/bin/supercronic" | sha256sum -c - \
 && chmod 755 /usr/local/bin/supercronic

COPY crontab /etc/crontab
COPY excludes.txt /etc/backup/excludes.txt
COPY scripts/ /scripts/
RUN chmod 755 /scripts/*.sh && chmod 644 /scripts/lib.sh /etc/crontab /etc/backup/excludes.txt

ENTRYPOINT ["/sbin/tini", "--"]
CMD ["/scripts/entrypoint.sh"]
