# PROGRESS (overnight run, started 2026-11-12)

Plan: `PLAN-2.md`. Decisions and accepted risks: `DECISIONS.md` part "Phase 2".

- [x] Survey of server and apps
- [x] PLAN-2.md written (commit 124d932)
- [x] Phase A: ntfy + Portal notifications panel + /api/notify + NOTIFY.md (commit b4b8e38)
- [x] Phase B infra: VictoriaMetrics, node_exporter, vmalert, Alertmanager -> ntfy, Caddy LAN-IP supervisor (commit f12fb21)
- [x] Phase C infra: restic backups, restore tests, RESTORE.md (commit 3bf22db)
- [x] Portal B+C: container metrics, uptime bars, /graphs, Backups panel (commit 8d3c50c)
- [x] Integration + DoD verification: all 11 PASS (commits 124d932, 014cbbe)
- [x] Security review done; fixes A1 (status.json arithmetic injection) and B1-B6 (Portal mount scope, proxy socket volume, SSE CR injection, clean child env, VM admin keys, pinned socket-proxy) (commit f525cbd)
- [x] Docs: README, LEARNING (sections 9-19, line references re-verified), DECISIONS (#31-#79), stale paths in NOTIFY/RESTORE/PLAN-2, MORNING.md (gitignored) (final commit "Docs: architecture, notifications, monitoring, backups, learning notes; all phases complete")
- [x] Final commit (same commit; not pushed)

ALL PHASES COMPLETE
