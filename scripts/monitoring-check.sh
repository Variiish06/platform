#!/bin/sh
# Validate the monitoring config BEFORE (re)loading it, like a unit test:
#   1. vmalert -dryRun parses every rule in monitoring/alerts/*.yml
#      (syntax, PromQL/MetricsQL, annotation templates) and exits.
#   2. amtool check-config validates monitoring/alertmanager.yml.
#   3. VictoriaMetrics -dryRun validates monitoring/scrape.yml.
# Uses the same pinned images as compose.monitoring.yml, in throwaway
# containers with no network. Exit code is non-zero if anything is wrong.
set -eu
cd "$(dirname "$0")/.."

img() { sed -n "s|^ *image: *\(.*$1:[^ ]*\).*|\1|p" compose.monitoring.yml | head -n 1; }
VMALERT=$(img vmalert)
AM=$(img alertmanager)
VM=$(img victoria-metrics)

echo "== vmalert rules ($VMALERT)"
docker run --rm --network none -v "$PWD/monitoring/alerts:/alerts:ro" \
    "$VMALERT" -dryRun -rule='/alerts/*.yml' -loggerLevel=WARN
echo "rules OK"

echo "== alertmanager.yml ($AM)"
# The token file only has to exist for the check; use an empty stand-in.
docker run --rm --network none --entrypoint /bin/sh \
    -v "$PWD/monitoring/alertmanager.yml:/etc/alertmanager/alertmanager.yml:ro" \
    "$AM" -c 'touch /tmp/t && sed "s|/etc/alertmanager/ntfy-token|/tmp/t|" /etc/alertmanager/alertmanager.yml > /tmp/am.yml && amtool check-config /tmp/am.yml'

echo "== scrape.yml ($VM)"
docker run --rm --network none -v "$PWD/monitoring/scrape.yml:/scrape.yml:ro" \
    "$VM" -dryRun -promscrape.config=/scrape.yml -loggerLevel=WARN
echo "scrape config OK"
