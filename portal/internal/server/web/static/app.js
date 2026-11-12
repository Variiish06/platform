// Dashboard live updates. No framework: the server renders HTML, and this
// file only (1) listens to the event stream and swaps content in, and
// (2) handles the restart button.
(function () {
  'use strict';
  const $ = (sel, root) => (root || document).querySelector(sel);

  // ---- Bars -------------------------------------------------------------
  // Bar widths arrive as data-pct attributes and are applied here, because
  // the Content-Security-Policy forbids inline style="" attributes.
  function applyBars(root) {
    root.querySelectorAll('[data-pct]').forEach(function (el) {
      const p = Math.max(0, Math.min(100, parseFloat(el.dataset.pct) || 0));
      el.style.width = p + '%';
    });
  }

  // ---- CPU sparkline ----------------------------------------------------
  const cpuHistory = [];
  function drawSpark(value) {
    cpuHistory.push(Math.max(0, Math.min(100, value)));
    if (cpuHistory.length > 60) cpuHistory.shift();
    const line = $('.spark polyline');
    if (!line || cpuHistory.length < 2) return;
    // Scale to the busiest recent moment (at least 25%) so a mostly idle
    // machine still draws a readable line.
    const top = Math.max(25, Math.max.apply(null, cpuHistory));
    const step = 120 / 59;
    const start = 60 - cpuHistory.length;
    line.setAttribute('points', cpuHistory.map(function (v, i) {
      return ((start + i) * step).toFixed(1) + ',' + (34 - (v / top) * 32).toFixed(1);
    }).join(' '));
  }

  // ---- System panel: patch values in place ------------------------------
  // The cells keep their DOM nodes, so the bars can animate between values.
  function updateStats(stats) {
    stats.forEach(function (s) {
      const cell = $('.stat[data-key="' + s.k + '"]');
      if (!cell) return;
      cell.dataset.state = s.st;
      $('[data-f="v"]', cell).textContent = s.v;
      $('[data-f="s"]', cell).textContent = s.s;
      const bar = $('.bar', cell);
      bar.classList.toggle('none', s.p < 0);
      $('i', bar).style.width = Math.max(0, Math.min(100, s.p)) + '%';
      if (s.k === 'cpu') drawSpark(s.p);
    });
  }

  // ---- Event stream ------------------------------------------------------
  const live = $('#live'), liveText = $('#live-text');
  function setLive(state, text) { live.dataset.state = state; liveText.textContent = text; }

  const busy = new Set(); // container ids with a restart in flight
  function markBusy() {
    document.querySelectorAll('.row').forEach(function (row) {
      row.classList.toggle('busy', busy.has(row.dataset.id));
    });
  }

  let source = null, failures = 0;
  function connect() {
    if (source) source.close();
    source = new EventSource('/events');
    source.onopen = function () { failures = 0; setLive('live', 'Live'); };
    source.onerror = function () {
      failures++;
      setLive(failures > 2 ? 'offline' : 'connecting', failures > 2 ? 'Offline' : 'Reconnecting');
      // EventSource retries by itself. But if the session ended, retrying
      // will never work: ask the server and go to the login page if so.
      if (failures === 2) {
        fetch('/api/session', { credentials: 'same-origin' }).then(function (r) {
          if (r.status === 401) location.href = '/login';
        }).catch(function () {});
      }
      if (source.readyState === EventSource.CLOSED) setTimeout(connect, 3000);
    };
    // "apps" and "containers" carry ready-made HTML from the server's
    // templates; "system" carries JSON for in-place patching.
    source.addEventListener('apps', function (e) { $('#apps').innerHTML = e.data; });
    source.addEventListener('containers', function (e) { $('#containers').innerHTML = e.data; markBusy(); });
    source.addEventListener('system', function (e) { updateStats(JSON.parse(e.data)); });
  }

  // Phones suspend background tabs and silently kill the connection.
  // Reconnect as soon as the page is visible again.
  document.addEventListener('visibilitychange', function () {
    if (document.visibilityState === 'visible' && (!source || source.readyState !== EventSource.OPEN)) connect();
  });

  // ---- Toast --------------------------------------------------------------
  const toastEl = $('#toast');
  let toastTimer;
  function toast(text, isError) {
    toastEl.textContent = text;
    toastEl.classList.toggle('error', !!isError);
    toastEl.classList.add('show');
    clearTimeout(toastTimer);
    toastTimer = setTimeout(function () { toastEl.classList.remove('show'); }, isError ? 6000 : 3500);
  }

  // ---- Restart button ------------------------------------------------------
  const dialog = $('#confirm');
  let pending = null;
  // One listener on the document (event delegation): the rows are replaced
  // on every update, so listeners on the buttons themselves would be lost.
  document.addEventListener('click', function (e) {
    const btn = e.target.closest('[data-restart]');
    if (!btn || busy.has(btn.dataset.restart)) return;
    const running = btn.dataset.running === 'true';
    pending = { id: btn.dataset.restart, name: btn.dataset.name, running: running };
    $('#confirm-title').textContent = (running ? 'Restart ' : 'Start ') + pending.name + '?';
    $('#confirm-text').textContent = running
      ? 'It will be unavailable for a few seconds while it restarts.'
      : 'This container is stopped. Starting it brings it back up.';
    $('#confirm-ok').textContent = running ? 'Restart' : 'Start';
    dialog.returnValue = 'cancel';
    dialog.showModal();
  });
  dialog.addEventListener('close', function () {
    if (dialog.returnValue !== 'ok' || !pending) return;
    const job = pending;
    pending = null;
    busy.add(job.id);
    markBusy();
    fetch('/api/containers/' + job.id + '/restart', { method: 'POST', credentials: 'same-origin' })
      .then(function (r) {
        if (r.status === 401) { location.href = '/login'; return; }
        if (!r.ok) throw new Error('status ' + r.status);
        toast(job.name + (job.running ? ' restarted' : ' started'));
      })
      .catch(function () {
        // Restarting the proxy or Portal itself cuts this very request,
        // so a network error here is not always a failure.
        toast('Couldn’t confirm ' + job.name + ' restarted. Check the list in a moment.', true);
      })
      .finally(function () { busy.delete(job.id); markBusy(); });
  });

  applyBars(document);
  // Seed the sparkline with the history the server put in the page.
  const spark = $('.spark');
  if (spark && spark.dataset.hist) spark.dataset.hist.split(',').forEach(function (v) { drawSpark(parseFloat(v) || 0); });
  connect();
})();
