(function () {
  'use strict';
  var WORDS = {ok: 'OK', lagging: 'LAGGING', dead: 'DEAD', full: 'FULL', slow: 'SLOW', unknown: 'UNKNOWN', green: 'GREEN', yellow: 'DEGRADED', red: 'DOWN'};
  var KEY = 'blobgate-admin-token';
  var token = '';
  try { token = sessionStorage.getItem(KEY) || ''; } catch (e) { token = ''; }

  // Everything that comes from the server is put in the page as text, never as markup.
  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) { e.className = cls; }
    if (text !== undefined) { e.textContent = text; }
    return e;
  }
  function pill(state) { return el('span', 'pill ' + state, WORDS[state] || state); }
  function size(n) {
    if (n === null || n === undefined) { return ''; }
    var u = ['B', 'KiB', 'MiB', 'GiB', 'TiB'], i = 0;
    while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
    return n.toFixed(i ? 1 : 0) + ' ' + u[i];
  }
  function age(s) {
    if (!s) { return ''; }
    if (s < 120) { return s + ' s'; }
    if (s < 7200) { return Math.round(s / 60) + ' min'; }
    return Math.round(s / 3600) + ' h';
  }
  function cell(row, text, cls) { row.appendChild(el('td', cls || '', text)); }

  function draw(data, detailed) {
    var overall = document.getElementById('overall');
    overall.className = 'overall ' + data.status;
    overall.textContent = (WORDS[data.status] || data.status) + (data.status === 'green' ? ': every node answers and has everything' : '');
    var root = document.getElementById('stores');
    root.textContent = '';
    (data.stores || []).forEach(function (st) {
      var box = el('section', 'store');
      var head = el('header');
      head.appendChild(el('h2', '', st.name));
      head.appendChild(pill(st.state));
      box.appendChild(head);
      var wrap = el('div', 'node-list');
      var table = el('table');
      var hr = el('tr');
      var cols = detailed ? ['Node', 'State', 'Id', 'Answer', 'Waiting copies', 'Oldest', 'Free', 'Last error'] : ['Node', 'State'];
      cols.forEach(function (c) { hr.appendChild(el('th', '', c)); });
      table.appendChild(hr);
      (st.nodes || []).forEach(function (n) {
        var r = el('tr');
        cell(r, n.name);
        var td = el('td'); td.appendChild(pill(n.state)); r.appendChild(td);
        if (detailed) {
          cell(r, n.id);
          cell(r, n.state === 'unknown' ? '' : (n.latency_ms ? n.latency_ms + ' ms' : '<1 ms'));
          cell(r, n.pending ? String(n.pending) : '');
          cell(r, age(n.oldest_age_seconds));
          cell(r, n.free === null || n.free === undefined ? '' : size(n.free) + ' of ' + size(n.total));
          cell(r, n.error || '', 'err');
        }
        table.appendChild(r);
      });
      wrap.appendChild(table);
      box.appendChild(wrap);
      root.appendChild(box);
    });
  }

  function say(text) { document.getElementById('message').textContent = text; }

  function load() {
    var detailed = token !== '';
    var url = detailed ? '/v1/admin/status' : '/status.json';
    var opts = {cache: 'no-store'};
    if (detailed) { opts.headers = {Authorization: 'Bearer ' + token}; }
    fetch(url, opts).then(function (r) {
      if (r.status === 401 && detailed) {
        token = '';
        try { sessionStorage.removeItem(KEY); } catch (e) { /* no storage: nothing to forget */ }
        say('The admin token was refused: showing the public view.');
        return load();
      }
      if (!r.ok) { throw new Error('HTTP ' + r.status); }
      return r.json().then(function (d) { draw(d, detailed); if (detailed) { say('Details shown with the admin token.'); } });
    }).catch(function (e) {
      var o = document.getElementById('overall');
      o.className = 'overall unknown';
      o.textContent = 'The service did not answer (' + e.message + ')';
    });
  }

  document.getElementById('auth').addEventListener('submit', function (ev) {
    ev.preventDefault();
    var v = document.getElementById('token').value.trim();
    if (v === '') { return; }
    token = v;
    document.getElementById('token').value = '';
    try { sessionStorage.setItem(KEY, token); } catch (e) { /* the token stays in memory for this page */ }
    load();
  });
  document.getElementById('forget').addEventListener('click', function () {
    token = '';
    try { sessionStorage.removeItem(KEY); } catch (e) { /* nothing kept */ }
    say('The token is forgotten.');
    load();
  });
  load();
  setInterval(load, 5000);
}());
