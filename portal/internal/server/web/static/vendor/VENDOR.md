# Vendored third-party files

These files are embedded into the Portal binary (go:embed) and served from
`/static/vendor/`. The browser never fetches anything from a CDN at runtime.

| File | Package | Version | License | SHA-256 |
|---|---|---|---|---|
| `uPlot.iife.min.js` | [uPlot](https://github.com/leeoniya/uPlot) | 1.6.32 | MIT (`uPlot.LICENSE`) | `19c8d4c6ad88929a79f4ae49d6f7161566dfd0ba3d15cc495e974f787eb78f1f` |
| `uPlot.min.css` | uPlot | 1.6.32 | MIT | `df630c6a8d6f8eeaff264b50f73ce5b114f646ffd9a0bb74f049b0a00135fa04` |
| `uPlot.LICENSE` | uPlot | 1.6.32 | - | `8f989229699b4fe2f1a0432d0e9edc338a8a911e250e2d1b01ecd770a5f5b1bd` |

Downloaded 2026-11-12 from
`https://cdn.jsdelivr.net/npm/uplot@1.6.32/dist/<file>` and cross-checked
byte-for-byte against `https://unpkg.com/uplot@1.6.32/dist/<file>` (same hashes).

Verify: `sha256sum portal/internal/server/web/static/vendor/uPlot*`

To upgrade: download the new pinned version from both CDNs, compare the
hashes, replace the files, update this table. uPlot draws on <canvas> and sets
styles through the DOM (element.style), which the Content-Security-Policy
allows; it injects no <style> or <script> elements.
