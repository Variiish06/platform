// Light/dark theme. Loaded synchronously from <head> (the CSP forbids
// inline scripts), so the stored choice is applied before the first paint
// and the page never flashes the wrong theme. Without a stored choice the
// page follows the system setting (prefers-color-scheme, in app.css).
// Also keeps the header clock current. Every storage access is guarded:
// private windows may refuse it.
(function () {
  'use strict';
  var KEY = 'portal.theme';
  var root = document.documentElement;
  try {
    var stored = localStorage.getItem(KEY);
    if (stored === 'light' || stored === 'dark') root.dataset.theme = stored;
  } catch (e) { /* no storage: follow the system */ }

  var mq = window.matchMedia ? window.matchMedia('(prefers-color-scheme: light)') : null;
  function current() { return root.dataset.theme || (mq && mq.matches ? 'light' : 'dark'); }
  function announce() {
    // graphs.js listens to redraw its charts in the new colours.
    document.dispatchEvent(new CustomEvent('portal:theme', { detail: current() }));
  }

  function pad(n) { return (n < 10 ? '0' : '') + n; }

  document.addEventListener('DOMContentLoaded', function () {
    var btn = document.getElementById('theme-toggle');
    if (btn) {
      var label = function () {
        var next = current() === 'dark' ? 'light' : 'dark';
        btn.textContent = next === 'light' ? 'Light' : 'Dark';
        btn.setAttribute('aria-label', 'Switch to the ' + next + ' theme');
      };
      btn.hidden = false;
      label();
      btn.addEventListener('click', function () {
        var next = current() === 'dark' ? 'light' : 'dark';
        root.dataset.theme = next;
        try { localStorage.setItem(KEY, next); } catch (e) { /* ignore */ }
        label();
        announce();
      });
      if (mq && mq.addEventListener) mq.addEventListener('change', function () { label(); announce(); });
    }

    var clock = document.getElementById('clock');
    if (clock) {
      var tick = function () {
        var d = new Date();
        clock.textContent = pad(d.getHours()) + ':' + pad(d.getMinutes());
        clock.setAttribute('datetime', d.toISOString());
      };
      tick();
      setInterval(tick, 10000);
    }
  });
})();
