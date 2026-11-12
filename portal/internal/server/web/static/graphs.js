// Graphs page. Each <figure data-graph="cpu"> asks the server for
// GET /api/graphs/cpu?range=24h and draws the answer with uPlot (vendored,
// see static/vendor/VENDOR.md). The server runs a fixed PromQL query
// against VictoriaMetrics; this file never builds a query itself.
//
// uPlot wants columnar data: data[0] = x values (unix seconds), data[1..]
// = one array per line, null for gaps. That is exactly what the API sends.
(function () {
  'use strict';
  const $ = (sel, root) => (root || document).querySelector(sel);
  const REFRESH_MS = 45000;
  // Line colours for the dark theme; the first is the site's mint accent.
  const COLORS = ['#5eead4', '#60a5fa', '#fbbf24', '#f472b6', '#a78bfa', '#4ade80',
    '#fb923c', '#f87171', '#22d3ee', '#e879f9', '#facc15', '#94a3b8'];

  // ---- Units ---------------------------------------------------------------
  function bytes(v) {
    const units = ['B', 'KB', 'MB', 'GB', 'TB'];
    let i = 0;
    let n = Math.abs(v);
    while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
    return (v < 0 ? '-' : '') + (n >= 100 || i === 0 ? n.toFixed(0) : n.toFixed(1)) + ' ' + units[i];
  }
  const FORMAT = {
    percent: function (v) { return (Math.abs(v) < 10 ? v.toFixed(1) : v.toFixed(0)) + '%'; },
    bytes: bytes,
    bytes_s: function (v) { return bytes(v) + '/s'; },
    celsius: function (v) { return v.toFixed(0) + '°C'; },
    seconds: function (v) {
      if (v < 1) return (v * 1000).toFixed(v < 0.01 ? 1 : 0) + ' ms';
      if (v < 120) return v.toFixed(v < 10 ? 1 : 0) + ' s';
      return (v / 60).toFixed(0) + ' min';
    },
    number: function (v) { return v.toFixed(2); }
  };
  function fmt(unit, v) {
    if (v == null || !isFinite(v)) return '–';
    return (FORMAT[unit] || FORMAT.number)(v);
  }

  // ---- Range selector --------------------------------------------------------
  // The choice is remembered per browser; storage may be unavailable
  // (private mode), so every access is guarded.
  let range = '24h';
  try { range = localStorage.getItem('portal.graphs.range') || range; } catch (e) { /* ignore */ }
  const rangeBtns = document.querySelectorAll('.range-btn');
  if (![].some.call(rangeBtns, function (b) { return b.dataset.range === range; })) range = '24h';
  function markRange() {
    rangeBtns.forEach(function (b) { b.setAttribute('aria-pressed', String(b.dataset.range === range)); });
  }
  rangeBtns.forEach(function (b) {
    b.addEventListener('click', function () {
      if (range === b.dataset.range) return;
      range = b.dataset.range;
      try { localStorage.setItem('portal.graphs.range', range); } catch (e) { /* ignore */ }
      markRange();
      refreshAll();
    });
  });
  markRange();

  // ---- VictoriaMetrics banner ------------------------------------------------
  const banner = $('#vm-banner');
  function setVM(ok) { if (banner && banner.dataset.vm !== 'off') banner.hidden = ok; }

  // ---- Charts ------------------------------------------------------------------
  const charts = [];
  document.querySelectorAll('[data-graph]').forEach(function (fig) {
    charts.push({
      fig: fig, name: fig.dataset.graph, unit: fig.dataset.unit,
      plotEl: $('.chart-plot', fig), emptyEl: $('.chart-empty', fig), legendEl: $('.chart-legend', fig),
      plot: null, key: '', data: null, labels: []
    });
  });

  function plotSize(c) {
    const w = Math.max(200, Math.floor(c.plotEl.clientWidth || c.fig.clientWidth - 24));
    return { width: w, height: w < 420 ? 170 : 200 };
  }

  // x-axis labels: uPlot formats times in the browser's zone (IST on the
  // owner's devices). Short formats so a phone-width axis is not crowded.
  function axisStyle(extra) {
    return Object.assign({
      stroke: '#7d8896',
      grid: { stroke: 'rgba(255,255,255,.06)', width: 1 },
      ticks: { stroke: 'rgba(255,255,255,.1)', width: 1, size: 4 },
      font: '11px system-ui, -apple-system, sans-serif'
    }, extra || {});
  }

  function makePlot(c, res) {
    if (c.plot) { c.plot.destroy(); c.plot = null; }
    const unit = c.unit;
    const series = [{}].concat(res.series.map(function (s, i) {
      return {
        label: s.label, stroke: COLORS[i % COLORS.length], width: 1.6,
        // points: uPlot's default draws dots only when data is sparse, so
        // isolated samples (a fresh install, a gap) are still visible.
        spanGaps: false,
        value: function (u, v) { return fmt(unit, v); }
      };
    }));
    const opts = Object.assign(plotSize(c), {
      series: series,
      legend: { show: false }, // our own legend below (latest values, theme)
      cursor: { points: { size: 6 }, drag: { x: false, y: false } },
      scales: {
        x: { time: true },
        y: {
          range: function (u, min, max) {
            const lo = Math.min(0, min);
            let hi = max > 0 ? max * 1.12 : 1;
            // Percent charts that come near 100 get a fixed 0-100 scale.
            if (unit === 'percent' && max <= 100 && max > 60) hi = 100;
            return [lo, hi];
          }
        }
      },
      axes: [
        axisStyle(),
        axisStyle({ size: 58, values: function (u, vals) { return vals.map(function (v) { return fmt(unit, v); }); } })
      ],
      hooks: { setCursor: [function (u) { renderLegend(c, u.cursor.idx); }] }
    });
    c.plot = new uPlot(opts, res.data, c.plotEl);
  }

  // Legend: one row per line with its colour and the value under the
  // cursor, or the latest value when the cursor is not on the chart.
  function renderLegend(c, idx) {
    if (!c.data) return;
    const rows = c.legendEl.children;
    for (let i = 0; i < c.labels.length; i++) {
      const vals = c.data[i + 1];
      let v = null;
      if (idx != null) v = vals[idx];
      else for (let j = vals.length - 1; j >= 0; j--) if (vals[j] != null) { v = vals[j]; break; }
      const valEl = rows[i] && rows[i].lastChild;
      if (valEl) valEl.textContent = fmt(c.unit, v);
    }
  }
  function buildLegend(c) {
    c.legendEl.textContent = '';
    c.labels.forEach(function (label, i) {
      const li = document.createElement('li');
      const sw = document.createElement('i');
      sw.className = 'sw';
      sw.style.background = COLORS[i % COLORS.length]; // CSSOM, allowed by the CSP
      const name = document.createElement('span');
      name.className = 'lg-name';
      name.textContent = label; // textContent: labels are never parsed as HTML
      const val = document.createElement('span');
      val.className = 'lg-val';
      li.append(sw, name, val);
      c.legendEl.appendChild(li);
    });
  }

  function hasValues(res) {
    for (let i = 1; i < res.data.length; i++) {
      if (res.data[i].some(function (v) { return v != null; })) return true;
    }
    return false;
  }

  function showEmpty(c, text) {
    c.fig.classList.add('is-empty');
    c.emptyEl.textContent = text;
    if (c.plot) { c.plot.destroy(); c.plot = null; c.key = ''; }
    c.legendEl.textContent = '';
    c.data = null;
  }

  function load(c) {
    const r = range;
    return fetch('/api/graphs/' + encodeURIComponent(c.name) + '?range=' + encodeURIComponent(r), { credentials: 'same-origin' })
      .then(function (resp) {
        if (resp.status === 401) { location.href = '/login'; throw new Error('signed out'); }
        return resp.json().then(function (body) { return { status: resp.status, body: body }; });
      })
      .then(function (x) {
        if (r !== range) return; // the user switched range meanwhile
        if (x.status === 503) { setVM(false); showEmpty(c, 'No data: VictoriaMetrics not reachable'); return; }
        if (x.status !== 200) { showEmpty(c, 'Could not load this chart'); return; }
        setVM(true);
        const res = x.body;
        if (!res.series.length || !hasValues(res)) { showEmpty(c, 'No data yet'); return; }
        c.fig.classList.remove('is-empty');
        const key = r + '|' + res.series.map(function (s) { return s.label; }).join('\u0000');
        c.data = res.data;
        c.labels = res.series.map(function (s) { return s.label; });
        if (c.plot && c.key === key) {
          c.plot.setData(res.data); // same lines: update in place, keeps the DOM
        } else {
          buildLegend(c);
          makePlot(c, res); // first draw, or the set of lines changed (top N)
          c.key = key;
        }
        renderLegend(c, null);
      })
      .catch(function () {
        if (r === range) showEmpty(c, 'Could not load this chart');
      });
  }

  let lastRefresh = 0;
  function refreshAll() {
    lastRefresh = Date.now();
    charts.forEach(load);
  }

  // Auto-refresh only while the tab is visible: a background tab on a
  // phone should cost nothing.
  setInterval(function () {
    if (document.visibilityState === 'visible') refreshAll();
  }, REFRESH_MS);
  document.addEventListener('visibilitychange', function () {
    if (document.visibilityState === 'visible' && Date.now() - lastRefresh > REFRESH_MS) refreshAll();
  });

  // Charts follow their container's width (rotation, window resize).
  if ('ResizeObserver' in window) {
    const ro = new ResizeObserver(function () {
      charts.forEach(function (c) { if (c.plot) c.plot.setSize(plotSize(c)); });
    });
    charts.forEach(function (c) { ro.observe(c.plotEl); });
  }

  refreshAll();
})();
