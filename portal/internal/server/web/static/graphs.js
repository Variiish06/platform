// Graphs page. Each <figure data-graph="cpu"> asks the server for
// GET /api/graphs/cpu?range=24h and draws the answer with uPlot (vendored,
// see static/vendor/VENDOR.md). The server runs a fixed PromQL query
// against VictoriaMetrics; this file never builds a query itself.
//
// uPlot wants columnar data: data[0] = x values (unix seconds), data[1..]
// = one array per line, null for gaps. That is exactly what the API sends.
// Colours come from the CSS custom properties, so the charts follow the
// light/dark theme and are redrawn when it changes.
(function () {
  'use strict';
  const $ = (sel, root) => (root || document).querySelector(sel);
  const REFRESH_MS = 45000;
  const MONO = '11px ui-monospace, "SF Mono", "Cascadia Mono", Consolas, "Liberation Mono", monospace';

  // ---- Theme ---------------------------------------------------------------
  let theme = {};
  function readTheme() {
    const cs = getComputedStyle(document.documentElement);
    const v = function (n) { return cs.getPropertyValue(n).trim(); };
    theme = {
      ink: v('--ink'), ink2: v('--ink-2'), ink3: v('--ink-3'), rule: v('--rule'), rule2: v('--rule-2'), accent: v('--accent'),
      // Lines: six colours, then the same six dashed (series 7-12).
      lines: [v('--c1'), v('--c2'), v('--c3'), v('--c4'), v('--c5'), v('--c6')],
      // Stacked memory: the same shades as the dashboard's memory bar.
      areas: [v('--m1'), v('--m2'), v('--m3'), v('--m4'), v('--m5'), v('--m6'), v('--m-cache'), v('--bg')]
    };
  }
  readTheme();
  function lineStyle(i) {
    return { color: theme.lines[i % 6], dash: i >= 6 ? [5, 3] : null };
  }

  // ---- Units ---------------------------------------------------------------
  function bytes(v) {
    const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
    let i = 0;
    let n = Math.abs(v);
    while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
    return (v < 0 ? '-' : '') + (i === 0 ? n.toFixed(0) : n.toFixed(1)) + ' ' + units[i];
  }
  const FORMAT = {
    percent: function (v) { return (Math.abs(v) < 10 ? v.toFixed(1) : v.toFixed(0)) + '%'; },
    bytes: bytes,
    bytes_s: function (v) { return bytes(v) + '/s'; },
    celsius: function (v) { return v.toFixed(0) + ' °C'; },
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
      fig: fig, name: fig.dataset.graph, unit: fig.dataset.unit, stacked: fig.dataset.stacked === '1',
      plotEl: $('.chart-plot', fig), emptyEl: $('.chart-empty', fig), legendEl: $('.chart-legend', fig),
      plot: null, key: '', raw: null, labels: [], res: null
    });
  });

  function plotSize(c) {
    const w = Math.max(200, Math.floor(c.plotEl.clientWidth || c.fig.clientWidth));
    return { width: w, height: c.stacked ? 240 : (w < 420 ? 150 : 170) };
  }

  function axisStyle(extra) {
    return Object.assign({
      stroke: theme.ink3,
      grid: { stroke: theme.rule, width: 1 },
      ticks: { stroke: theme.rule, width: 1, size: 3 },
      font: MONO
    }, extra || {});
  }

  // Stacked charts draw running totals: series k is the sum of series
  // 1..k, and the band between k and k-1 is filled. The legend still
  // shows each part's own value (from c.raw).
  function cumulative(data) {
    const out = [data[0]];
    let acc = null;
    for (let i = 1; i < data.length; i++) {
      acc = data[i].map(function (v, j) {
        const below = acc ? acc[j] : 0;
        return v == null ? below : (below == null ? v : below + v);
      });
      out.push(acc);
    }
    return out;
  }

  function makePlot(c, res) {
    if (c.plot) { c.plot.destroy(); c.plot = null; }
    const unit = c.unit;
    const series = [{}].concat(res.series.map(function (s, i) {
      const base = { label: s.label, spanGaps: false, value: function (u, v) { return fmt(unit, v); } };
      if (c.stacked) {
        const fill = theme.areas[i % theme.areas.length];
        return Object.assign(base, { stroke: theme.rule2, width: 1, fill: i === 0 ? fill : undefined, points: { show: false } });
      }
      const ls = lineStyle(i);
      return Object.assign(base, { stroke: ls.color, width: 1, dash: ls.dash || undefined });
    }));
    const bands = [];
    if (c.stacked) {
      for (let i = 2; i <= res.series.length; i++) bands.push({ series: [i, i - 1], fill: theme.areas[(i - 1) % theme.areas.length] });
    }
    const opts = Object.assign(plotSize(c), {
      series: series,
      bands: bands,
      legend: { show: false }, // our own legend below (latest values)
      cursor: { points: { size: 5, fill: theme.accent }, drag: { x: false, y: false } },
      scales: {
        x: { time: true },
        y: {
          range: function (u, min, max) {
            const lo = Math.min(0, min);
            let hi = max > 0 ? max * (c.stacked ? 1.02 : 1.12) : 1;
            // Percent charts that come near 100 get a fixed 0-100 scale.
            if (unit === 'percent' && max <= 100 && max > 60) hi = 100;
            return [lo, hi];
          }
        }
      },
      axes: [
        axisStyle(),
        axisStyle({
          values: function (u, vals) { return vals.map(function (v) { return fmt(unit, v); }); },
          // Wide enough for the longest label ("390.6 KiB/s"): ~6.7 px per
          // monospace character at 11 px, plus the ticks and a gap.
          size: function (u, values) {
            const n = (values || []).reduce(function (m, s) { return Math.max(m, String(s).length); }, 4);
            return Math.ceil(n * 6.7) + 14;
          }
        })
      ],
      hooks: { setCursor: [function (u) { renderLegend(c, u.cursor.idx); }] }
    });
    c.plot = new uPlot(opts, c.stacked ? cumulative(res.data) : res.data, c.plotEl);
  }

  // Legend: one row per series with its swatch and the value under the
  // cursor, or the latest value when the cursor is not on the chart.
  function renderLegend(c, idx) {
    if (!c.raw) return;
    const rows = c.legendEl.children;
    for (let i = 0; i < c.labels.length; i++) {
      const vals = c.raw[i + 1];
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
      if (c.stacked) {
        sw.className = 'sw area';
        sw.style.background = theme.areas[i % theme.areas.length]; // CSSOM, allowed by the CSP
        sw.style.boxShadow = 'inset 0 0 0 1px ' + theme.rule2;
      } else {
        const ls = lineStyle(i);
        if (ls.dash) { sw.className = 'sw dash'; sw.style.borderTopColor = ls.color; } else sw.style.background = ls.color;
      }
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
    c.raw = null;
    c.res = null;
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
        c.raw = res.data;
        c.res = res;
        c.labels = res.series.map(function (s) { return s.label; });
        if (c.plot && c.key === key) {
          c.plot.setData(c.stacked ? cumulative(res.data) : res.data); // same lines: update in place
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

  // Theme switched (button or system): redraw with the new colours.
  document.addEventListener('portal:theme', function () {
    readTheme();
    charts.forEach(function (c) { if (c.res) { buildLegend(c); makePlot(c, c.res); renderLegend(c, null); } });
  });

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
