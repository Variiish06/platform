// Dashboard live updates. No framework: the server renders HTML, and this
// file only (1) listens to the event stream and swaps content in, and
// (2) handles sorting, the restart/stop/start and the backup buttons.
(function () {
  'use strict';
  const $ = (sel, root) => (root || document).querySelector(sel);
  const $$ = (sel, root) => (root || document).querySelectorAll(sel);

  // ---- Bars -------------------------------------------------------------
  // Widths arrive as data-pct attributes and are applied here, because
  // the Content-Security-Policy forbids inline style="" attributes.
  function applyBars(root) {
    $$('[data-pct]', root).forEach(function (el) {
      const p = Math.max(0, Math.min(100, parseFloat(el.dataset.pct) || 0));
      // The memory bar is a flex row: grow by share so 1 px gaps never
      // push the total past 100%.
      if (el.parentElement && el.parentElement.classList.contains('mbar')) el.style.flex = p + ' 0 0';
      else el.style.width = p + '%';
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
      return ((start + i) * step).toFixed(1) + ',' + (17 - (v / top) * 16).toFixed(1);
    }).join(' '));
  }

  // ---- System readings: patch values in place ----------------------------
  function updateStats(stats) {
    stats.forEach(function (s) {
      const cell = $('[data-key="' + s.k + '"]');
      if (!cell) return;
      if (cell.classList.contains('rd')) cell.dataset.state = s.st;
      const v = $('[data-f="v"]', cell), sub = $('[data-f="s"]', cell);
      if (v) v.textContent = s.v;
      if (sub) sub.textContent = s.s;
      const meter = $('.meter', cell);
      if (meter) {
        meter.classList.toggle('none', s.p < 0);
        $('i', meter).style.width = Math.max(0, Math.min(100, s.p)) + '%';
      }
      if (s.k === 'cpu') drawSpark(parseFloat(s.v) || 0);
    });
  }

  // ---- Relative times -----------------------------------------------------
  // "5 min ago" goes stale while the page is open; recompute it from the
  // timestamp the server put in data-ts. Same rules as ago() in notify.go.
  function relTime(ts) {
    const s = Math.max(0, Date.now() / 1000 - ts);
    if (s < 60) return 'just now';
    if (s < 3600) return Math.floor(s / 60) + ' min ago';
    if (s < 86400) return Math.floor(s / 3600) + ' h ago';
    return Math.floor(s / 86400) + ' d ago';
  }
  function updateTimes() {
    $$('time[data-ts]').forEach(function (el) {
      el.textContent = relTime(parseInt(el.dataset.ts, 10) || 0);
    });
  }
  setInterval(updateTimes, 30000);

  // ---- Container table sorting --------------------------------------------
  // Stopped containers always sit at the bottom; within each part the rows
  // follow the chosen column. The choice survives live updates and reloads.
  let sortBy = { key: 'project', dir: 'asc' };
  try { sortBy = JSON.parse(localStorage.getItem('portal.containers.sort')) || sortBy; } catch (e) { /* ignore */ }
  function sortContainers() {
    const body = $('#containers tbody');
    if (!body) return;
    const rows = [].slice.call(body.rows);
    const numeric = sortBy.key === 'cpu' || sortBy.key === 'mem';
    const sign = sortBy.dir === 'asc' ? 1 : -1;
    rows.sort(function (a, b) {
      const ra = a.dataset.run === 'true', rb = b.dataset.run === 'true';
      if (ra !== rb) return ra ? -1 : 1;
      let c = 0;
      if (numeric) c = (parseFloat(a.dataset[sortBy.key]) || 0) - (parseFloat(b.dataset[sortBy.key]) || 0);
      else c = (a.dataset[sortBy.key] || '').localeCompare(b.dataset[sortBy.key] || '');
      if (c === 0) c = sign * (a.dataset.name || '').localeCompare(b.dataset.name || '');
      return sign * c;
    });
    rows.forEach(function (r) { body.appendChild(r); });
    $$('#containers .sort').forEach(function (b) {
      if (b.dataset.sort === sortBy.key) b.setAttribute('aria-sort-dir', sortBy.dir);
      else b.removeAttribute('aria-sort-dir');
      const th = b.closest('th');
      if (th) th.setAttribute('aria-sort', b.dataset.sort === sortBy.key ? (sortBy.dir === 'asc' ? 'ascending' : 'descending') : 'none');
    });
  }
  document.addEventListener('click', function (e) {
    const b = e.target.closest('.sort');
    if (!b) return;
    const key = b.dataset.sort;
    if (sortBy.key === key) sortBy.dir = sortBy.dir === 'asc' ? 'desc' : 'asc';
    else sortBy = { key: key, dir: key === 'cpu' || key === 'mem' ? 'desc' : 'asc' };
    try { localStorage.setItem('portal.containers.sort', JSON.stringify(sortBy)); } catch (err) { /* ignore */ }
    sortContainers();
  });

  // ---- Swapping in a fragment ------------------------------------------------
  // Open <details> (memory groups, older notifications) stay open across
  // updates: remember them by data-k and reopen after the swap.
  function swap(el, html, fade) {
    if (!el) return;
    const open = new Set();
    $$('details[open][data-k]', el).forEach(function (d) { open.add(d.dataset.k); });
    const focusedSort = document.activeElement && el.contains(document.activeElement) && document.activeElement.dataset.sort;
    el.innerHTML = html;
    $$('details[data-k]', el).forEach(function (d) { if (open.has(d.dataset.k)) d.open = true; });
    applyBars(el);
    updateTimes();
    if (focusedSort) { const b = $('.sort[data-sort="' + focusedSort + '"]', el); if (b) b.focus(); }
    if (fade) {
      el.classList.remove('fresh');
      void el.offsetWidth; // restart the animation
      el.classList.add('fresh');
    }
  }

  // ---- Event stream ------------------------------------------------------
  const live = $('#live'), liveText = $('#live-text');
  function setLive(state, text) { if (live) { live.dataset.state = state; liveText.textContent = text; } }

  // Keys like "container:<id>" or "app:<id>" with a request in flight.
  const busy = new Set();
  function markBusy() {
    $$('#containers tr[data-id]').forEach(function (row) {
      row.classList.toggle('busy', busy.has('container:' + row.dataset.id));
    });
    $$('#apps tr[data-app]').forEach(function (row) {
      row.classList.toggle('busy', busy.has('app:' + row.dataset.app));
    });
  }

  let source = null, failures = 0;
  function connect() {
    if (source) source.close();
    source = new EventSource('/events');
    source.onopen = function () { failures = 0; setLive('live', 'live'); };
    source.onerror = function () {
      failures++;
      setLive(failures > 2 ? 'offline' : 'connecting', failures > 2 ? 'offline' : 'reconnecting');
      // EventSource retries by itself. But if the session ended, retrying
      // will never work: ask the server and go to the login page if so.
      if (failures === 2) {
        fetch('/api/session', { credentials: 'same-origin' }).then(function (r) {
          if (r.status === 401) location.href = '/login';
        }).catch(function () {});
      }
      if (source.readyState === EventSource.CLOSED) setTimeout(connect, 3000);
    };
    // Most events carry ready-made HTML from the server's templates;
    // "system" carries JSON for in-place patching.
    source.addEventListener('status', function (e) { swap($('#status'), e.data, true); });
    source.addEventListener('apps', function (e) { swap($('#apps'), e.data, false); markBusy(); });
    source.addEventListener('memory', function (e) { swap($('#memory'), e.data, false); });
    source.addEventListener('containers', function (e) { swap($('#containers'), e.data, false); sortContainers(); markBusy(); });
    source.addEventListener('system', function (e) { updateStats(JSON.parse(e.data)); });
    source.addEventListener('notify', function (e) { swap($('#notify'), e.data, true); });
    source.addEventListener('backup', function (e) { swap($('#backup'), e.data, true); });
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

  // ---- Restart / stop / start buttons ------------------------------------
  // Every action goes through the confirm dialog first. Stop is the one
  // that leaves something down, so its confirm button is red.
  const dialog = $('#confirm');
  const okBtn = $('#confirm-ok');
  let pending = null;
  const words = {
    restart: { title: 'Restart ', ok: 'Restart', done: ' restarted' },
    start: { title: 'Start ', ok: 'Start', done: ' started' },
    stop: { title: 'Stop ', ok: 'Stop', done: ' stopped' }
  };
  function confirmText(job) {
    const app = job.kind === 'app';
    const many = app && job.containers.indexOf(',') >= 0;
    switch (job.action) {
      case 'restart':
        return 'It will be unavailable for a few seconds while it restarts.';
      case 'start':
        return (app ? 'Starts ' + job.containers + '.' : 'This container is stopped. Starting it brings it back up.') +
          ' The alert silences Portal set when it was stopped end a few minutes later.';
      default:
        return (app ? 'Stops ' + job.containers + '. ' : '') + (many ? 'They stay' : 'It stays') +
          ' stopped until you start ' + (many ? 'them' : 'it') +
          ' again here. Alerts about ' + (many ? 'them' : 'it') + ' are silenced in Alertmanager meanwhile (for up to 30 days).';
    }
  }
  // One listener on the document (event delegation): the rows are replaced
  // on every update, so listeners on the buttons themselves would be lost.
  document.addEventListener('click', function (e) {
    const btn = e.target.closest('[data-ctl]');
    if (!btn) return;
    const job = { action: btn.dataset.ctl, kind: btn.dataset.kind, id: btn.dataset.target,
      name: btn.dataset.name, containers: btn.dataset.containers || '' };
    if (!words[job.action] || busy.has(job.kind + ':' + job.id)) return;
    pending = job;
    $('#confirm-title').textContent = words[job.action].title + job.name + '?';
    $('#confirm-text').textContent = confirmText(job);
    okBtn.textContent = words[job.action].ok;
    okBtn.classList.toggle('danger', job.action === 'stop');
    dialog.returnValue = 'cancel';
    dialog.showModal();
  });
  dialog.addEventListener('close', function () {
    if (dialog.returnValue !== 'ok' || !pending) return;
    const job = pending;
    pending = null;
    const key = job.kind + ':' + job.id;
    busy.add(key);
    markBusy();
    // A stopped container is started with Docker's restart (the socket
    // proxy allows restart and stop, nothing else).
    const url = job.kind === 'app'
      ? '/api/apps/' + encodeURIComponent(job.id) + '/' + job.action
      : '/api/containers/' + encodeURIComponent(job.id) + '/' + (job.action === 'stop' ? 'stop' : 'restart');
    fetch(url, { method: 'POST', credentials: 'same-origin' })
      .then(function (r) {
        if (r.status === 401) { location.href = '/login'; return; }
        return r.text().then(function (t) {
          let j = {};
          try { j = JSON.parse(t); } catch (err) { j = { error: t.trim() }; }
          if (!r.ok) { toast(j.error || ('Failed (' + r.status + ')'), true); return; }
          if (j.warning) toast(job.name + words[job.action].done + '. ' + j.warning, true);
          else toast(job.name + words[job.action].done);
        });
      })
      .catch(function () {
        // Restarting the proxy or Portal itself cuts this very request,
        // so a network error here is not always a failure.
        toast('Couldn’t confirm ' + job.name + words[job.action].done + '. Check the list in a moment.', true);
      })
      .finally(function () { busy.delete(key); markBusy(); });
  });

  // ---- Backup buttons -------------------------------------------------------
  // Both only ask the server to drop a request file; the backup container
  // does the work. The panel then shows "Requested…" and, a few seconds
  // later, "Backing up…" as status.json changes (pushed over /events).
  function backupPost(path, body, label) {
    $$('[data-backup]').forEach(function (b) { b.disabled = true; });
    return fetch(path, {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: body ? JSON.stringify(body) : '{}'
    }).then(function (r) {
      if (r.status === 401) { location.href = '/login'; return; }
      return r.json().catch(function () { return {}; }).then(function (j) {
        if (r.ok) { toast(label + ' requested. It starts within a few seconds.'); return; }
        toast(j.error || ('Request failed (' + r.status + ')'), true);
        $$('[data-backup]').forEach(function (b) { b.disabled = false; });
      });
    }).catch(function () {
      toast('Could not reach Portal.', true);
      $$('[data-backup]').forEach(function (b) { b.disabled = false; });
    });
  }

  const restoreDialog = $('#restore-dialog');
  const restoreSnap = $('#restore-snap');
  document.addEventListener('click', function (e) {
    const btn = e.target.closest('[data-backup]');
    if (!btn || btn.disabled) return;
    if (btn.dataset.backup === 'run') {
      backupPost('/api/backup/run', null, 'Backup');
      return;
    }
    // Fill the snapshot list from the panel's inert <template>: "Latest"
    // first, then the snapshots status.json listed (ids already validated
    // on the server; option text is set as text, not HTML).
    restoreSnap.textContent = '';
    const latest = document.createElement('option');
    latest.value = 'latest';
    latest.textContent = 'Latest';
    restoreSnap.appendChild(latest);
    const tpl = $('#snap-options');
    if (tpl) restoreSnap.appendChild(tpl.content.cloneNode(true));
    restoreDialog.returnValue = 'cancel';
    restoreDialog.showModal();
  });
  if (restoreDialog) {
    restoreDialog.addEventListener('close', function () {
      if (restoreDialog.returnValue !== 'ok') return;
      backupPost('/api/backup/restore-staging', { snapshot: restoreSnap.value || 'latest' }, 'Restore to staging');
    });
  }

  applyBars(document);
  sortContainers();
  // Seed the sparkline with the history the server put in the page.
  const spark = $('.spark');
  if (spark && spark.dataset.hist) spark.dataset.hist.split(',').forEach(function (v) { drawSpark(parseFloat(v) || 0); });
  connect();
})();
