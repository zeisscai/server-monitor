/* 服务器监控 — 前端交互（原生 JS，无外部依赖） */
(function () {
  'use strict';

  var $ = function (s, r) { return (r || document).querySelector(s); };
  var $$ = function (s, r) { return Array.prototype.slice.call((r || document).querySelectorAll(s)); };

  function level(v) { return v >= 85 ? 'danger' : v >= 60 ? 'warn' : 'ok'; }
  function pct(v) { return (Math.round((v || 0) * 10) / 10).toFixed(1); }

  function pad(n) { return n < 10 ? '0' + n : '' + n; }

  function fmtTime(ms) {
    if (!ms) return '—';
    var d = new Date(ms);
    return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate()) + ' ' +
           pad(d.getHours()) + ':' + pad(d.getMinutes()) + ':' + pad(d.getSeconds());
  }

  function fmtShort(ms, span) {
    var d = new Date(ms);
    if (span <= 2 * 86400000) return pad(d.getHours()) + ':' + pad(d.getMinutes());
    if (span <= 62 * 86400000) return pad(d.getMonth() + 1) + '-' + pad(d.getDate());
    return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate());
  }

  function fmtLocalInput(ms) {
    var d = new Date(ms);
    return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate()) + 'T' +
           pad(d.getHours()) + ':' + pad(d.getMinutes());
  }

  function relTime(ms) {
    if (!ms) return '—';
    var diff = Date.now() - ms;
    if (diff < 5000) return '刚刚';
    var s = Math.floor(diff / 1000);
    if (s < 60) return s + ' 秒前';
    var m = Math.floor(s / 60);
    if (m < 60) return m + ' 分钟前';
    var h = Math.floor(m / 60);
    if (h < 24) return h + ' 小时前';
    return Math.floor(h / 24) + ' 天前';
  }

  /* 运行时长：由开机时刻推算，只保留两级单位 */
  function fmtUptime(sec) {
    if (!sec || sec <= 0) return '未知';
    var d = Math.floor(sec / 86400);
    var h = Math.floor((sec % 86400) / 3600);
    var m = Math.floor((sec % 3600) / 60);
    if (d > 0) return d + ' 天 ' + h + ' 小时';
    if (h > 0) return h + ' 小时 ' + m + ' 分';
    if (m > 0) return m + ' 分钟';
    return '不足 1 分钟';
  }

  function esc(s) {
    return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }

  function sourceLabel(src) {
    if (src === 'remote') return '<span class="src src-remote">上报</span>';
    return '<span class="src src-local">本机</span>';
  }

  /* ---------------- 相对时间 / 运行时长：每秒刷新 ---------------- */
  function tickRelative() {
    $$('[data-ts]').forEach(function (el) {
      var ms = Number(el.getAttribute('data-ts')) || 0;
      var prefix = el.getAttribute('data-ts-prefix');
      el.textContent = (prefix || '') + relTime(ms);
    });
  }

  function tickUptime() {
    $$('[data-boot]').forEach(function (el) {
      var boot = Number(el.getAttribute('data-boot')) || 0;
      if (!boot) return;
      el.textContent = fmtUptime((Date.now() - boot) / 1000);
    });
  }

  /* ---------------- 面板 ---------------- */
  function metricBlock(label, value) {
    return '<div class="metric"><span class="metric-label">' + label + '</span>' +
           '<span class="metric-val">' + pct(value) + '%</span></div>' +
           '<div class="bar"><i class="fill" data-level="' + level(value) +
           '" style="width:' + Math.max(0, Math.min(100, value)) + '%"></i></div>';
  }

  function sysLine(s) {
    var parts = ['<span class="srv-os" title="' + esc(s.os) + '">' +
      (s.os ? esc(s.os) : '系统信息未知') + '</span>'];
    if (s.kernel) {
      parts.push('<span class="dotsep">·</span><span class="mono srv-kernel" title="' +
        esc(s.kernel) + '">' + esc(s.kernel) + '</span>');
    }
    if (s.boot_at) {
      parts.push('<span class="dotsep">·</span><span>运行 <b data-boot="' + s.boot_at + '">' +
        fmtUptime(s.uptime) + '</b></span>');
    }
    return '<div class="srv-sys">' + parts.join('') + '</div>';
  }

  function cardHTML(s) {
    var name = esc(s.name || s.uuid);
    return '<a class="srv-card" href="/server/' + esc(s.uuid) + '" data-uuid="' + esc(s.uuid) + '">' +
      '<div class="srv-head"><span class="srv-name" title="' + name + '">' + name + '</span>' +
      '<span class="badge ' + (s.is_self ? 'badge-self' : 'badge-remote') + '">' +
      (s.is_self ? '本机' : '外来上报') + '</span></div>' +
      metricBlock('CPU 占用', s.cpu) + metricBlock('内存占用', s.mem) +
      sysLine(s) +
      '<div class="srv-foot"><span class="muted" data-ts-prefix="更新于 " data-ts="' + s.ts + '">更新于 ' +
      relTime(s.ts) + '</span><span class="chev">›</span></div></a>';
  }

  function initPanel() {
    var grid = $('#grid');
    if (!grid) return;
    var empty = $('#grid-empty');
    var count = $('#srv-count');
    var busy = false;

    function refresh() {
      if (busy || document.hidden) return;
      busy = true;
      fetch('/api/overview', { credentials: 'same-origin' })
        .then(function (r) { return r.json(); })
        .then(function (d) {
          if (!d || !d.ok) return;
          var list = d.servers || [];
          grid.innerHTML = list.map(cardHTML).join('');
          if (empty) empty.hidden = list.length > 0;
          if (count) count.textContent = list.length;
          tickRelative();
          tickUptime();
        })
        .catch(function () {})
        .then(function () { busy = false; });
    }

    var btn = $('#refresh-btn');
    if (btn) btn.addEventListener('click', function (e) { e.preventDefault(); refresh(); });

    setInterval(refresh, 5000);
    tickRelative();
    tickUptime();
  }

  /* ---------------- 服务器详情 ---------------- */
  function drawChart(canvas, tip, points, from, to) {
    var box = canvas.parentNode;
    var cssW = box.clientWidth || canvas.clientWidth || 600;
    var cssH = 320;
    var dpr = window.devicePixelRatio || 1;
    canvas.width = Math.round(cssW * dpr);
    canvas.height = Math.round(cssH * dpr);
    canvas.style.height = cssH + 'px';

    var ctx = canvas.getContext('2d');
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, cssW, cssH);

    var padL = 46, padR = 14, padT = 14, padB = 30;
    var w = cssW - padL - padR;
    var h = cssH - padT - padB;
    if (w <= 10 || h <= 10) return;

    var span = Math.max(1, to - from);
    function X(t) { return padL + (t - from) / span * w; }
    function Y(v) { return padT + (1 - Math.max(0, Math.min(100, v)) / 100) * h; }

    ctx.font = '11px ui-monospace, SFMono-Regular, Menlo, Consolas, monospace';
    ctx.textBaseline = 'middle';

    // 横向网格 + Y 轴刻度
    for (var v = 0; v <= 100; v += 25) {
      var y = Y(v);
      ctx.strokeStyle = v === 0 ? '#2b3a4d' : '#1b2534';
      ctx.lineWidth = 1;
      ctx.beginPath();
      ctx.moveTo(padL, Math.round(y) + 0.5);
      ctx.lineTo(padL + w, Math.round(y) + 0.5);
      ctx.stroke();
      ctx.fillStyle = '#6b7d92';
      ctx.textAlign = 'right';
      ctx.fillText(v + '%', padL - 9, y);
    }

    // 纵向网格 + X 轴刻度
    var ticks = 5;
    for (var i = 0; i <= ticks; i++) {
      var t = from + span * i / ticks;
      var x = X(t);
      ctx.strokeStyle = '#1b2534';
      ctx.beginPath();
      ctx.moveTo(Math.round(x) + 0.5, padT);
      ctx.lineTo(Math.round(x) + 0.5, padT + h);
      ctx.stroke();
      ctx.fillStyle = '#6b7d92';
      ctx.textAlign = i === 0 ? 'left' : (i === ticks ? 'right' : 'center');
      ctx.fillText(fmtShort(t, span), x, padT + h + 16);
    }

    if (!points.length) return;

    function line(key, color, fillTop) {
      ctx.beginPath();
      for (var i = 0; i < points.length; i++) {
        var px = X(points[i].t), py = Y(points[i][key]);
        if (i === 0) ctx.moveTo(px, py); else ctx.lineTo(px, py);
      }
      if (fillTop) {
        var grad = ctx.createLinearGradient(0, padT, 0, padT + h);
        grad.addColorStop(0, fillTop);
        grad.addColorStop(1, 'rgba(255,123,114,0)');
        ctx.save();
        ctx.lineTo(X(points[points.length - 1].t), padT + h);
        ctx.lineTo(X(points[0].t), padT + h);
        ctx.closePath();
        ctx.fillStyle = grad;
        ctx.fill();
        ctx.restore();
        ctx.beginPath();
        for (var j = 0; j < points.length; j++) {
          var jx = X(points[j].t), jy = Y(points[j][key]);
          if (j === 0) ctx.moveTo(jx, jy); else ctx.lineTo(jx, jy);
        }
      }
      ctx.strokeStyle = color;
      ctx.lineWidth = 1.7;
      ctx.lineJoin = 'round';
      ctx.stroke();
    }

    line('mem', '#58a6ff');
    line('cpu', '#ff7b72', 'rgba(255,123,114,.16)');

    // 单点情况下补一个圆点
    if (points.length === 1) {
      ctx.fillStyle = '#ff7b72';
      ctx.beginPath();
      ctx.arc(X(points[0].t), Y(points[0].cpu), 3, 0, Math.PI * 2);
      ctx.fill();
    }

    // 悬停提示
    function onMove(ev) {
      if (!tip) return;
      var rect = canvas.getBoundingClientRect();
      var mx = ev.clientX - rect.left;
      var best = 0, bestD = Infinity;
      for (var i = 0; i < points.length; i++) {
        var d = Math.abs(X(points[i].t) - mx);
        if (d < bestD) { bestD = d; best = i; }
      }
      var p = points[best];
      tip.hidden = false;
      tip.innerHTML = '<div class="muted">' + fmtTime(p.t) + '</div>' +
        '<div><span style="color:#ff7b72">CPU</span> <b>' + pct(p.cpu) + '%</b></div>' +
        '<div><span style="color:#58a6ff">内存</span> <b>' + pct(p.mem) + '%</b></div>';
      var left = Math.min(Math.max(X(p.t) - tip.offsetWidth / 2, 4), cssW - tip.offsetWidth - 4);
      tip.style.left = left + 'px';
      tip.style.top = '6px';
    }
    canvas.onmousemove = onMove;
    canvas.onmouseleave = function () { if (tip) tip.hidden = true; };
  }

  function statCard(k, v, cls) {
    return '<div class="stat"><div class="k">' + k + '</div><div class="v ' + (cls || '') + '">' + v + '</div></div>';
  }

  function initDetail() {
    var meta = $('#detail-meta');
    if (!meta) return;

    var uuid = meta.getAttribute('data-uuid');
    var canvas = $('#chart');
    var tip = $('#chart-tip');
    var statsEl = $('#stats');
    var tbody = $('#records-body');
    var chartEmpty = $('#chart-empty');
    var recordsEmpty = $('#records-empty');
    var recordsNote = $('#records-note');
    var rangeNote = $('#range-note');
    var customBox = $('#custom-range');
    var fromInput = $('#range-from');
    var toInput = $('#range-to');

    var current = null;
    var activeRange = '30d';

    function rangeFor(name) {
      var to = Date.now(), from;
      var d = new Date();
      if (name === '1h') from = to - 3600000;
      else if (name === '24h') from = to - 86400000;
      else if (name === '7d') from = to - 7 * 86400000;
      else { d.setMonth(d.getMonth() - 1); from = d.getTime(); }
      return { from: from, to: to };
    }

    function load(from, to) {
      return fetch('/api/history?uuid=' + encodeURIComponent(uuid) + '&from=' + from + '&to=' + to, { credentials: 'same-origin' })
        .then(function (r) { return r.json(); })
        .then(function (d) {
          if (!d || !d.ok) return;
          current = d;
          render(d);
        })
        .catch(function () {});
    }

    function render(d) {
      var pts = d.points || [];
      var recs = d.records || [];
      var st = d.stats || {};

      if (chartEmpty) chartEmpty.hidden = pts.length > 0;
      canvas.style.display = pts.length ? 'block' : 'none';
      if (tip) tip.hidden = true;
      if (pts.length) drawChart(canvas, tip, pts, d.from, d.to);

      statsEl.innerHTML =
        statCard('记录条数', st.count || 0) +
        statCard('CPU 平均', pct(st.cpu_avg) + '%', 'cpu') +
        statCard('CPU 峰值', pct(st.cpu_max) + '%', 'cpu') +
        statCard('内存平均', pct(st.mem_avg) + '%', 'mem') +
        statCard('内存峰值', pct(st.mem_max) + '%', 'mem');

      if (recordsEmpty) recordsEmpty.hidden = recs.length > 0;
      tbody.innerHTML = recs.map(function (r) {
        return '<tr><td class="num">' + fmtTime(r.ts) + '</td>' +
          '<td class="num">' + pct(r.cpu) + '%</td>' +
          '<td class="num">' + pct(r.mem) + '%</td>' +
          '<td>' + sourceLabel(r.source) + '</td></tr>';
      }).join('');
      if (recordsNote) recordsNote.textContent = recs.length ? ('最多显示最近 200 条，共 ' + (st.count || 0) + ' 条') : '';

      if (rangeNote) {
        rangeNote.textContent = fmtTime(d.from) + '  ~  ' + fmtTime(d.to);
      }
    }

    function applyRange(name) {
      activeRange = name;
      $$('.range').forEach(function (b) {
        b.classList.toggle('active', b.getAttribute('data-range') === name);
      });
      if (name === 'custom') {
        customBox.hidden = false;
        var r = current ? { from: current.from, to: current.to } : rangeFor('30d');
        fromInput.value = fmtLocalInput(r.from);
        toInput.value = fmtLocalInput(r.to);
        return;
      }
      customBox.hidden = true;
      var rg = rangeFor(name);
      load(rg.from, rg.to);
    }

    $$('.range').forEach(function (b) {
      b.addEventListener('click', function () { applyRange(b.getAttribute('data-range')); });
    });

    var applyBtn = $('#range-apply');
    if (applyBtn) {
      applyBtn.addEventListener('click', function () {
        var f = fromInput.value ? new Date(fromInput.value).getTime() : 0;
        var t = toInput.value ? new Date(toInput.value).getTime() : Date.now();
        if (!f || !t) return;
        if (t < f) { var tmp = f; f = t; t = tmp; }
        load(f, t);
      });
    }

    var resizeTimer = null;
    window.addEventListener('resize', function () {
      if (!current) return;
      clearTimeout(resizeTimer);
      resizeTimer = setTimeout(function () {
        if (current && (current.points || []).length) drawChart(canvas, tip, current.points, current.from, current.to);
      }, 160);
    });

    applyRange('30d');
  }

  /* ---------------- 设置页 ---------------- */
  function initSettings() {
    var form = $('.settings-form');
    if (!form) return;

    var receive = document.getElementById('receive_enabled');
    var api = document.getElementById('api_enabled');
    var report = document.getElementById('report_enabled');

    function setDep(name, on) {
      var dep = $('.dep[data-dep="' + name + '"]');
      if (!dep) return;
      dep.classList.toggle('dim', !on);
      $$('input, select', dep).forEach(function (el) {
        if (el.readOnly) return;
        el.disabled = !on;
      });
    }

    function sync() {
      setDep('receive', !!(receive && receive.checked));
      setDep('api', !!(api && api.checked));
      setDep('report', !!(report && report.checked));
      // 端口由「数据接收」与「查询 API」共用，任一开启就应可编辑，
      // 否则只开 API 时端口框会被接收开关一起置灰。
      var port = $('input[name="receive_port"]');
      if (port) {
        port.disabled = !((receive && receive.checked) || (api && api.checked));
      }
    }

    [receive, api, report].forEach(function (cb) {
      if (cb) cb.addEventListener('change', sync);
    });
    sync();

    $$('[data-copy]').forEach(function (btn) {
      btn.addEventListener('click', function () {
        var el = document.getElementById(btn.getAttribute('data-copy'));
        if (!el || !el.value) return;
        var old = btn.textContent;
        function done() {
          btn.textContent = '已复制';
          setTimeout(function () { btn.textContent = old; }, 1400);
        }
        function fallback() {
          var ro = el.hasAttribute('readonly');
          el.removeAttribute('readonly');
          el.select();
          el.setSelectionRange(0, el.value.length);
          try { document.execCommand('copy'); done(); } catch (e) { /* 忽略 */ }
          if (ro) el.setAttribute('readonly', 'readonly');
          window.getSelection().removeAllRanges();
        }
        if (navigator.clipboard && window.isSecureContext) {
          navigator.clipboard.writeText(el.value).then(done, fallback);
        } else {
          fallback();
        }
      });
    });
  }

  /* ---------------- 登录页 ---------------- */
  function initLogin() {
    var form = $('.login-form');
    if (!form) return;
    form.addEventListener('submit', function () {
      var btn = $('button[type="submit"]', form);
      if (btn) { btn.disabled = true; btn.textContent = '登录中…'; }
      setTimeout(function () { btn.disabled = false; btn.textContent = '登 录'; }, 6000);
    });
  }

  document.addEventListener('DOMContentLoaded', function () {
    initLogin();
    initPanel();
    initDetail();
    initSettings();
    // 相对时间与运行时长统一由一个秒级定时器推进，面板与详情页都受益。
    tickRelative();
    tickUptime();
    setInterval(function () { tickRelative(); tickUptime(); }, 1000);
  });
})();
