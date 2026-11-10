// macclean web UI — vanilla JS, no build step, served from the binary.
// Philosophy carried over from the TUI: real paths, real sizes, nothing
// deleted automatically. Trash actions are the only mutations and they
// always go through a confirmation (typed, for database files).

'use strict';

// ---- tiny helpers ----

const $ = (sel, el) => (el || document).querySelector(sel);
const main = () => $('#main');

function esc(s) {
  return String(s).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
}
function fmtBytes(n) {
  if (n == null) return '';
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0, v = n;
  while (v >= 1024 && i < 4) { v /= 1024; i++; }
  return (i === 0 ? v : v.toFixed(1)) + ' ' + u[i];
}
function fmtWhen(unix) {
  if (!unix) return '';
  const d = new Date(unix * 1000);
  return d.toLocaleDateString(undefined, {year: 'numeric', month: 'short', day: 'numeric'});
}
function fmtAgo(unix) {
  const s = Math.max(1, Math.floor(Date.now() / 1000 - unix));
  if (s < 3600) return Math.floor(s / 60) + ' min ago';
  if (s < 86400) return Math.floor(s / 3600) + ' h ago';
  const d = Math.floor(s / 86400);
  return d + (d === 1 ? ' day ago' : ' days ago');
}

async function api(path, opts) {
  const o = opts || {};
  const init = {method: o.method || 'GET', headers: {}};
  if (o.body !== undefined) {
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(o.body);
  }
  try {
    const resp = await fetch(path, init);
    let data = null;
    try { data = await resp.json(); } catch (e) { /* non-JSON error body */ }
    return {status: resp.status, data};
  } catch (e) {
    return {status: 0, data: {error: 'network error: ' + e.message}};
  }
}

let toastTimer = null;
function toast(msg, isErr) {
  const t = $('#toast');
  t.textContent = msg;
  t.className = 'show' + (isErr ? ' err' : '');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { t.className = ''; }, isErr ? 6000 : 3500);
}

// ---- modal ----

function closeModal() { $('#overlay').className = ''; }

function openModalHTML(html) {
  $('#modal').innerHTML = html + `<div class="actions"><button onclick="closeModal()">Close</button></div>`;
  $('#overlay').className = 'show';
}
$('#overlay') && $('#overlay').addEventListener('click', e => { if (e.target.id === 'overlay') closeModal(); });

function confirmModal({title, bodyHTML, confirmText, danger, typed}) {
  return new Promise(resolve => {
    const m = $('#modal');
    m.innerHTML = `
      <h3>${esc(title)}</h3>
      ${bodyHTML}
      ${typed ? `<p class="dim" style="font-size:13px">Type <code>trash</code> to confirm — these look like database files and may be in use.</p>
      <input type="text" id="typedword" placeholder="trash" autocomplete="off">` : ''}
      <div class="actions">
        <button id="mCancel">Cancel</button>
        <button id="mOk" class="${danger ? 'danger' : 'primary'}" disabled>${esc(confirmText || 'Confirm')}</button>
      </div>`;
    $('#overlay').className = 'show';
    const ok = $('#mOk');
    $('#mCancel').onclick = () => { closeModal(); resolve(false); };
    ok.onclick = () => { closeModal(); resolve(true); };
    if (typed) {
      const inp = $('#typedword');
      inp.oninput = () => { ok.disabled = inp.value.trim() !== 'trash'; };
      setTimeout(() => inp.focus(), 50);
    } else {
      ok.disabled = false;
      setTimeout(() => ok.focus(), 50);
    }
  });
}

// ---- trash flow (the only mutation anywhere) ----

async function trashPaths(items) {
  // items: [{path, size}] — sizes optional, for the confirm list
  const paths = items.map(i => i.path);
  const total = items.reduce((n, i) => n + (i.size || 0), 0);
  const ok = await confirmModal({
    title: `Move ${paths.length === 1 ? '1 item' : paths.length + ' items'} to Trash?`,
    bodyHTML: `<div class="paths">${esc(paths.join('\n'))}</div>
      <p class="dim" style="margin:4px 0 0">${fmtBytes(total)} · moved to the Finder Trash — recoverable until emptied.</p>`,
    confirmText: 'Move to Trash', danger: true,
  });
  if (!ok) return;
  let r = await api('/api/trash', {method: 'POST', body: {paths}});
  if (r.status === 412 && r.data && r.data.needDbConfirm) {
    const again = await confirmModal({
      title: 'Database files selected',
      bodyHTML: `<p class="dim">Duplicate databases are often intentional backups, and a live database must never be moved casually.</p>
        <div class="paths">${esc(r.data.needDbConfirm.join('\n'))}</div>`,
      confirmText: 'Move to Trash anyway', danger: true, typed: true,
    });
    if (!again) return;
    r = await api('/api/trash', {method: 'POST', body: {paths, confirmedDb: true}});
  }
  if (!r.data) { toast('trash failed: no response', true); return; }
  const d = r.data;
  if (d.moved && d.moved.length) {
    for (const p of d.moved) document.querySelectorAll(`tr[data-path="${CSS.escape(p)}"]`).forEach(tr => {
      tr.classList.add('struck');
      tr.querySelectorAll('button').forEach(b => (b.disabled = true));
    });
    toast(`Moved ${d.moved.length} item${d.moved.length === 1 ? '' : 's'} to the Trash`);
    loadHeader();
  }
  (d.failed || []).forEach(f => toast(`failed: ${f.path} — ${f.error}`, true));
}

async function reveal(path) {
  const r = await api('/api/reveal', {method: 'POST', body: {path}});
  if (r.status !== 200) toast('reveal failed: ' + ((r.data && r.data.error) || r.status), true);
}

// action buttons used in every table
function actionsHTML(path) {
  return `<button class="small" onclick="reveal(${JSON.stringify(path).replace(/"/g, '&quot;')})" title="Show in Finder">reveal</button>
    <button class="small danger" onclick="trashPaths([{path:${JSON.stringify(path).replace(/"/g, '&quot;')}}])">trash</button>`;
}

// ---- progress + job polling ----

let pollTimer = null;
function stopPoll() { if (pollTimer) { clearInterval(pollTimer); pollTimer = null; } }

function setProgress(label, frac) {
  const box = $('#progressbox');
  if (!box) return;
  $('#plabel').textContent = label || '';
  const pct = Math.round((frac || 0) * 100);
  $('#ppct').textContent = pct + '%';
  $('#pbar').style.width = pct + '%';
}

function progressHTML() {
  return `<div id="progressbox">
    <div class="plabel"><span id="plabel">starting…</span><span id="ppct">0%</span></div>
    <div class="bar"><div id="pbar"></div></div>
  </div>`;
}

async function startJob(kind, body, onDone) {
  const btn = $('#startbtn');
  if (btn) btn.disabled = true;
  const r = await api('/api/jobs', {method: 'POST', body: Object.assign({kind}, body || {})});
  if (r.status !== 202) {
    if (btn) btn.disabled = false;
    toast((r.data && r.data.error) || 'could not start scan', true);
    return;
  }
  mem.jobId[kind] = r.data.id;
  pollJob(r.data.id, onDone);
}

function pollJob(id, onDone) {
  stopPoll();
  pollTimer = setInterval(async () => {
    const r = await api('/api/jobs/' + id);
    if (r.status === 404) { stopPoll(); toast('scan result expired — run it again', true); return; }
    if (r.status !== 200) return;
    const j = r.data;
    setProgress(j.label, j.fraction);
    if (j.done) {
      stopPoll();
      const box = $('#progressbox');
      if (box) box.remove();
      if (j.error) toast(j.error, true);
      else onDone(j);
    }
  }, 700);
}

// ---- memory across screen switches ----

const mem = {
  results: {},   // kind -> last finished result payload
  jobId: {},     // kind -> id of the job that produced it
  analyzePath: '~',
  treePath: null,
};

function resumeOrPlaceholder(kind, startBtnHTML, onStart, onDone) {
  // Ask the server for the latest job of this kind: a reloaded page picks
  // up a running scan's progress or a finished result instantly.
  const box = $('#jobarea');
  box.innerHTML = `<div class="row" style="margin-bottom:12px">${startBtnHTML}</div><div id="result"></div>`;
  $('#startbtn').onclick = onStart;
  if (mem.results[kind]) renderResult(kind, mem.results[kind]);
  api('/api/jobs/latest?kind=' + kind).then(r => {
    if (r.status !== 200) return; // nothing yet — the start button is enough
    const j = r.data;
    mem.jobId[kind] = j.id;
    if (!j.done) {
      box.insertAdjacentHTML('afterbegin', progressHTML());
      $('#startbtn').disabled = true;
      pollJob(j.id, onDone);
    } else if (!j.error && j.result && !mem.results[kind]) {
      renderResult(kind, j.result);
    }
  });
}

function renderResult(kind, payload) {
  mem.results[kind] = payload;
  const r = $('#result');
  if (r) r.innerHTML = renderers[kind](payload);
}

const renderers = {}; // kind -> html(payload)

function doneHandler(kind) {
  return j => renderResult(kind, j.result);
}

// ---- header ----

async function loadHeader() {
  const r = await api('/api/overview');
  if (r.status !== 200) return;
  const v = r.data.volume;
  $('#ver').textContent = 'web · v' + r.data.version;
  const used = v.total - v.free;
  const pct = Math.round(used / v.total * 100);
  $('#diskline').innerHTML = `
    <span>${esc(v.name)}</span>
    <div class="bar" style="width:160px;height:7px"><div style="width:${pct}%"></div></div>
    <span class="mono">${fmtBytes(v.free)} free of ${fmtBytes(v.total)}</span>`;
}

// ---- screens ----

function screenOverview() {
  main().innerHTML = `
    <h1>Overview</h1>
    <p class="sub">Combined scan of your user folders with honest accounting: when the same file
    would be counted by several categories, it is credited once. Gross is the naive sum;
    <b>after overlap</b> is what you could actually get back.</p>
    <div id="jobarea"></div>`;
  resumeOrPlaceholder('dashboard',
    `<button id="startbtn" class="primary">Scan my disk</button> <span class="dim" style="font-size:12.5px">takes a minute or two — scans user folders, duplicates, stale deps, caches</span>`,
    () => { $('#jobarea').insertAdjacentHTML('afterbegin', progressHTML()); startJob('dashboard', {}, doneHandler('dashboard')); },
    doneHandler('dashboard'));
}

renderers.dashboard = d => {
  let vol = '';
  if (d.volume) {
    const used = d.volume.total - d.volume.free;
    const pct = Math.round(used / d.volume.total * 100);
    vol = `<div class="card"><div id="volume">
      <div><div class="big">${fmtBytes(used)}</div><div class="cap">used of ${fmtBytes(d.volume.total)} on ${esc(d.volume.name)}</div></div>
      <div class="bar"><div style="width:${pct}%"></div></div>
      <div style="text-align:right"><div class="big" style="color:var(--green)">${fmtBytes(d.volume.free)}</div><div class="cap">free</div></div>
    </div></div>`;
  }
  const cats = (d.categories || []).filter(c => c.actual > 0).map(c => {
    const k = JSON.stringify(c.kind).replace(/"/g, '&quot;');
    const n = JSON.stringify(c.name).replace(/"/g, '&quot;');
    return `
    <tr class="clickable" title="Show the files behind this category" onclick="showDetail(${k}, ${n})">
      <td>${esc(c.name)} <span class="badge kind">${esc(c.kind)}</span></td>
      <td class="num">${c.items}</td>
      <td class="num dim">${fmtBytes(c.gross)}</td>
      <td class="num">${fmtBytes(c.actual)}</td></tr>`;
  }).join('');
  const macosRows = (d.macos || []).map(it => `
    <tr data-path="${esc(it.path)}">
      <td>${esc(it.name)}<div class="dimmer mono" style="font-size:11.5px">${esc(it.path)}</div>${it.detail ? `<div class="dim" style="font-size:12px">${esc(it.detail)}</div>` : ''}</td>
      <td><span class="badge ${esc(it.safety)}">${esc(it.safety)}</span></td>
      <td class="num">${fmtBytes(it.size)}</td>
      <td style="white-space:nowrap">${actionsHTML(it.path)}</td></tr>`).join('');
  const cacheRows = (d.caches || []).map(c => `
    <tr data-path="${esc(c.path)}">
      <td>${esc(c.name)}<div class="dimmer mono" style="font-size:11.5px">${esc(c.path)}</div>${c.detail ? `<div class="dim" style="font-size:12px">${esc(c.detail)}</div>` : ''}</td>
      <td><span class="badge ${esc(c.safety)}">${esc(c.safety)}</span></td>
      <td class="num">${fmtBytes(c.size)}</td>
      <td style="white-space:nowrap">${actionsHTML(c.path)}</td></tr>`).join('');
  return `
    ${vol}
    <div class="card">
      <h2 style="margin-top:0">Potentially reclaimable <span class="dim" style="font-weight:400;font-size:12px">— click a row to see the files behind it</span></h2>
      <table><thead><tr><th>Category</th><th class="num">Items</th><th class="num">Gross</th><th class="num">After overlap</th></tr></thead>
      <tbody>${cats || '<tr><td colspan="4" class="empty">nothing found</td></tr>'}</tbody></table>
      <table style="margin-top:8px"><tbody>
        <tr><td class="dim">Gross (all categories summed)</td><td class="num dim">${fmtBytes(d.gross)}</td></tr>
        <tr><td><b>After overlap</b> — each physical file counted once</td><td class="num"><b>${fmtBytes(d.actual)}</b></td></tr>
      </tbody></table>
    </div>
    ${macosRows ? `<div class="card"><h2 style="margin-top:0">macOS cleanup targets</h2>
      <table><thead><tr><th>Item</th><th>Safety</th><th class="num">Size</th><th></th></tr></thead><tbody>${macosRows}</tbody></table>
      <p class="dim" style="font-size:12px;margin:8px 0 0">safe = regenerable · trash = reversible · destructive items are only offered in the terminal UI.</p></div>` : ''}
    ${cacheRows ? `<div class="card"><h2 style="margin-top:0">Developer caches</h2>
      <table><thead><tr><th>Cache</th><th>Safety</th><th class="num">Size</th><th></th></tr></thead><tbody>${cacheRows}</tbody></table>
      <p class="dim" style="font-size:12px;margin:8px 0 0">Deleting a cache here moves it to the Trash — it regenerates on next use.</p></div>` : ''}`;
};

// ---- folders ----

function screenFolders() {
  main().innerHTML = `
    <h1>Folders</h1>
    <p class="sub">Drill into any folder and see exactly where the bytes are. Click a folder row to
    descend; reveal or trash anything. The scan keeps a 15-minute cache, like the CLI.</p>
    <div id="jobarea">
      <div class="row" style="margin-bottom:12px">
        <input type="text" id="fpath" value="${esc(mem.analyzePath)}" placeholder="~ or /some/folder" spellcheck="false">
        <button id="startbtn" class="primary">Scan</button>
        <span class="dim" style="font-size:12.5px" id="fmeta"></span>
      </div>
      <div id="progresswrap"></div>
      <div id="result"></div>
    </div>`;
  $('#startbtn').onclick = startFolderScan;
  $('#fpath').addEventListener('keydown', e => { if (e.key === 'Enter') startFolderScan(); });
  if (mem.results.analyze) renderResult('analyze', mem.results.analyze);
  api('/api/jobs/latest?kind=analyze').then(r => {
    if (r.status !== 200) return;
    const j = r.data;
    mem.jobId.analyze = j.id;
    if (!j.done && !mem.results.analyze) {
      $('#progresswrap').innerHTML = progressHTML();
      pollJob(j.id, doneHandler('analyze'));
    }
  });
}

function startFolderScan() {
  const path = $('#fpath').value.trim() || '~';
  mem.analyzePath = path;
  mem.results.analyze = null;
  mem.treePath = null;
  $('#progresswrap').innerHTML = progressHTML();
  startJob('analyze', {path}, doneHandler('analyze'));
}

renderers.analyze = () => {
  loadTree(mem.treePath || (mem.results.analyze && mem.results.analyze.root.path));
  return '';
};

async function loadTree(path) {
  const id = mem.jobId.analyze;
  if (!id) return;
  const q = path ? '?path=' + encodeURIComponent(path) : '';
  const r = await api('/api/jobs/' + id + '/tree' + q);
  if (r.status !== 200) { toast('path not in this scan', true); return; }
  const t = r.data;
  mem.treePath = t.node.path;
  const crumbs = t.crumbs.map(c =>
    `<a href="#" onclick="loadTree(${JSON.stringify(c.path).replace(/"/g, '&quot;')});return false">${esc(c.name || '/')}</a>`
  ).join(' <span class="dimmer">/</span> ');
  const dirs = (t.dirs || []).map(d => `
    <tr class="clickable" onclick="loadTree(${JSON.stringify(d.path).replace(/"/g, '&quot;')})">
      <td>📁 ${esc(d.name)}</td>
      <td class="num">${fmtBytes(d.size)}</td>
      <td class="num dim">${d.files.toLocaleString()}</td>
      <td class="num dim">${d.dirs.toLocaleString()}</td>
      <td class="dim">${fmtWhen(d.modTime)}</td>
      <td></td></tr>`).join('');
  const files = (t.files || []).map(f => `
    <tr data-path="${esc(f.path)}">
      <td>${f.db ? '<span class="badge db">db</span> ' : ''}${f.dir ? '📁' : '📄'} ${esc(f.path.split('/').pop())}
        <div class="dimmer mono" style="font-size:11.5px">${esc(f.path)}</div></td>
      <td class="num">${fmtBytes(f.size)}</td>
      <td class="dim">${fmtWhen(f.modTime)}</td>
      <td class="dim">${fmtAgo(f.modTime)}</td>
      <td style="white-space:nowrap">${actionsHTML(f.path)}</td></tr>`).join('');
  $('#result').innerHTML = `
    <div class="card">
      <div class="row" style="margin-bottom:10px">
        <span class="mono" style="font-size:13px">${crumbs}</span>
        <span class="pill">${fmtBytes(t.node.size)} · ${t.totalFiles.toLocaleString()} files inside</span>
      </div>
      <table><thead><tr><th style="width:45%">Folder</th><th class="num">Size</th><th class="num">Files</th><th class="num">Dirs</th><th>Modified</th><th></th></tr></thead>
      <tbody>${dirs || '<tr><td colspan="6" class="empty">no subfolders</td></tr>'}</tbody></table>
    </div>
    <div class="card">
      <h2 style="margin-top:0">Largest files here <span class="dim" style="font-weight:400;font-size:12px">(top ${files ? (t.files || []).length : 0} of ${t.node.files ? t.node.files.toLocaleString() : 0})</span></h2>
      <table><thead><tr><th style="width:45%">File</th><th class="num">Size</th><th>Modified</th><th>Age</th><th></th></tr></thead>
      <tbody>${files || '<tr><td colspan="5" class="empty">no large files in this folder</td></tr>'}</tbody></table>
    </div>`;
}

// ---- dashboard category drill-down ----

const detailFullHint = {
  duplicates: 'the Duplicates screen lists every group',
  large: 'the Folders screen shows the full tree',
  old: 'the Folders screen shows the full tree',
  downloads: 'the Downloads screen lists everything',
  deps: 'the Stale deps screen lists every folder',
};

async function showDetail(kind, name) {
  const id = mem.jobId.dashboard;
  if (!id) { toast('run the overview scan first', true); return; }
  const q = '?kind=' + encodeURIComponent(kind) + (name ? '&name=' + encodeURIComponent(name) : '');
  const r = await api('/api/jobs/' + id + '/detail' + q);
  if (r.status === 404) { toast('scan result expired — re-run the overview scan', true); return; }
  if (r.status !== 200) { toast('detail failed: ' + ((r.data && r.data.error) || r.status), true); return; }
  renderDetailModal(kind, name, r.data);
}

function renderDetailModal(kind, name, d) {
  let body = '', sub = '';
  if (Array.isArray(d)) {
    // macos items or dev caches (per-item category rows)
    sub = `${d.length} item${d.length === 1 ? '' : 's'}`;
    body = `<table><tbody>${d.map(it => `
      <tr data-path="${esc(it.path)}">
        <td>${esc(it.name)}<div class="dimmer mono" style="font-size:11.5px">${esc(it.path)}</div>
          ${it.detail ? `<div class="dim" style="font-size:12px">${esc(it.detail)}</div>` : ''}</td>
        <td><span class="badge ${esc(it.safety)}">${esc(it.safety)}</span></td>
        <td class="num">${fmtBytes(it.size)}</td>
        <td style="white-space:nowrap">${actionsHTML(it.path)}</td></tr>`).join('')}</tbody></table>`;
  } else if (d.groups) {
    sub = `${d.totalCount} groups · ${fmtBytes(d.totalBytes)} reclaimable`;
    body = dupGroupsHTML(d.groups);
  } else if (d.deps) {
    sub = `${d.totalCount} folders · ${fmtBytes(d.totalBytes)}`;
    body = `<table><tbody>${d.deps.map(e => `
      <tr data-path="${esc(e.path)}">
        <td><span class="badge kind">${esc(e.kind)}</span> <span class="mono" style="font-size:12px">${esc(e.path)}</span>
          ${e.reinstall ? `<div class="dim mono" style="font-size:11.5px">reinstall: ${esc(e.reinstall)}</div>` : ''}</td>
        <td class="num">${fmtBytes(e.size)}</td>
        <td class="dim">${fmtAgo(e.lastActivity)}</td>
        <td style="white-space:nowrap">${actionsHTML(e.path)}</td></tr>`).join('')}</tbody></table>`;
  } else if (d.files) {
    sub = `${d.totalCount.toLocaleString()} files · ${fmtBytes(d.totalBytes)}`;
    body = `<table><tbody>${d.files.map(f => fileRow(f)).join('')}</tbody></table>`;
  } else {
    body = '<p class="empty">nothing in this category</p>';
  }
  if (d.truncated && detailFullHint[kind]) {
    sub += ` — showing the first rows; ${detailFullHint[kind]}`;
  }
  openModalHTML(`<h3>${esc(name || d.title || kind)} <span class="badge kind">${esc(kind)}</span></h3>
    <p class="dim" style="margin:0 0 10px;font-size:12.5px">${esc(sub)}</p>
    <div style="max-height:55vh;overflow:auto">${body}</div>`);
}

// Shared duplicate-group cards (Duplicates screen and dashboard detail).
function dupGroupsHTML(groups) {
  return (groups || []).map(g => {
    const rows = g.files.map((f, i) => `
      <tr data-path="${esc(f.path)}">
        <td>${i === g.keepIndex ? '<span class="badge keep">keep</span>' : ''}
          ${f.db ? '<span class="badge db">db</span>' : ''}
          <span class="mono" style="font-size:12px">${esc(f.path)}</span></td>
        <td class="num">${fmtBytes(f.size)}</td>
        <td class="dim">${fmtAgo(f.modTime)}</td>
        <td style="white-space:nowrap">${actionsHTML(f.path)}</td></tr>`).join('');
    const dups = g.files.filter((_, i) => i !== g.keepIndex);
    return `<div class="group">
      <div class="ghead">
        <span>${g.files.length} identical · ${fmtBytes(g.size)} each</span>
        <span class="grow">reclaimable ${fmtBytes(g.reclaimable)}</span>
        <button class="small danger" onclick='trashPaths(${JSON.stringify(dups.map(f => ({path: f.path, size: f.size}))).replace(/'/g, "&#39;")})'>trash all but keeper</button>
      </div>
      <table>${rows}</table>
    </div>`;
  }).join('');
}

// ---- duplicates ----

function screenDuplicates() {
  main().innerHTML = `
    <h1>Duplicates</h1>
    <p class="sub">Byte-identical files (size → partial hash → full SHA-256). The oldest copy is
    suggested as the keeper — a suggestion, never an action. Dev folders
    (node_modules, vendor, Pods) are excluded: those duplicates are structural and
    the safe cleanup unit is the whole folder, see Stale deps.</p>
    <div id="jobarea"></div>`;
  resumeOrPlaceholder('duplicates',
    `<button id="startbtn" class="primary">Find duplicates</button> <span class="dim" style="font-size:12.5px">scans your configured duplicate roots (see the TUI settings screen)</span>`,
    () => { $('#jobarea').insertAdjacentHTML('afterbegin', progressHTML()); startJob('duplicates', {}, doneHandler('duplicates')); },
    doneHandler('duplicates'));
}

renderers.duplicates = d => {
  const LIMIT = 25;
  const groups = d.groups || [];
  const cards = dupGroupsHTML(groups.slice(0, LIMIT));
  const more = groups.length > LIMIT
    ? `<p class="dim">… ${groups.length - LIMIT} more groups. Total reclaimable across all ${groups.length} groups: ${fmtBytes(d.reclaimable)}.</p>` : '';
  return `
    <div class="card row">
      <span>${groups.length} group${groups.length === 1 ? '' : 's'} · <b style="color:var(--green)">${fmtBytes(d.reclaimable)}</b> reclaimable</span>
      <span class="dim" style="font-size:12.5px">${d.scanned.toLocaleString()} files considered</span>
    </div>
    ${cards || '<div class="card empty">No byte-identical duplicates found.</div>'}
    ${more}`;
};

// ---- stale deps ----

function screenDeps() {
  main().innerHTML = `
    <h1>Stale deps</h1>
    <p class="sub">Project dependency folders and toolchain versions ranked by last activity.
    Untouched projects are suggested; recently used ones are kept. Every entry shows
    the exact reinstall command.</p>
    <div id="jobarea"></div>`;
  resumeOrPlaceholder('deps',
    `<button id="startbtn" class="primary">Scan projects</button> <span class="dim" style="font-size:12.5px">walks your project roots sizing node_modules / vendor / toolchains</span>`,
    () => { $('#jobarea').insertAdjacentHTML('afterbegin', progressHTML()); startJob('deps', {}, doneHandler('deps')); },
    doneHandler('deps'));
}

renderers.deps = d => {
  const stale = (d.stale || []).map(e => `
    <tr data-path="${esc(e.path)}">
      <td><span class="badge kind">${esc(e.kind)}</span>
        <span class="mono" style="font-size:12px">${esc(e.path)}</span>
        ${e.reinstall ? `<div class="dim mono" style="font-size:11.5px">reinstall: ${esc(e.reinstall)}</div>` : ''}</td>
      <td class="num">${fmtBytes(e.size)}</td>
      <td class="dim">${fmtAgo(e.lastActivity)}</td>
      <td style="white-space:nowrap">${actionsHTML(e.path)}</td></tr>`).join('');
  const fresh = (d.fresh || []).map(e => `
    <tr data-path="${esc(e.path)}">
      <td><span class="badge kind">${esc(e.kind)}</span> <span class="mono" style="font-size:12px">${esc(e.path)}</span></td>
      <td class="num">${fmtBytes(e.size)}</td>
      <td class="dim">${fmtAgo(e.lastActivity)}</td>
      <td style="white-space:nowrap">${actionsHTML(e.path)}</td></tr>`).join('');
  return `
    <div class="card row">
      <span>Stale (untouched ${d.staleDays}+ days): <b style="color:var(--green)">${fmtBytes(d.staleSize)}</b> in ${(d.stale || []).length} folders</span>
      <span class="dim" style="font-size:12.5px">fresh ${fmtBytes(d.freshSize)} · ${d.walkedDirs.toLocaleString()} dirs walked · roots: ${esc((d.roots || []).join(', '))}</span>
    </div>
    <div class="card">
      <h2 style="margin-top:0">Suggested — untouched projects</h2>
      <table><tbody>${stale || '<tr><td class="empty">nothing stale</td></tr>'}</tbody></table>
    </div>
    ${fresh ? `<div class="card"><details>
      <summary>Fresh — recently used (${(d.fresh || []).length} folders, ${fmtBytes(d.freshSize)}) — kept</summary>
      <table style="margin-top:8px"><tbody>${fresh}</tbody></table>
    </details></div>` : ''}`;
};

// ---- downloads ----

function screenDownloads() {
  main().innerHTML = `
    <h1>Downloads</h1>
    <p class="sub">~/Downloads categorized for cleanup: installers, archives and disk images you
    probably no longer need, plus old files — each counted once.</p>
    <div id="jobarea"></div>`;
  resumeOrPlaceholder('downloads',
    `<button id="startbtn" class="primary">Scan ~/Downloads</button>`,
    () => { $('#jobarea').insertAdjacentHTML('afterbegin', progressHTML()); startJob('downloads', {}, doneHandler('downloads')); },
    doneHandler('downloads'));
}

renderers.downloads = d => {
  const cats = (d.categories || []).map(c => `
    <tr><td>${esc(c.name)}</td><td class="num">${c.count.toLocaleString()}</td><td class="num">${fmtBytes(c.size)}</td></tr>`).join('');
  const cleanup = (d.cleanupFiles || []).map(f => fileRow(f)).join('');
  const large = (d.large || []).map(f => fileRow(f)).join('');
  const old = (d.old || []).map(f => fileRow(f)).join('');
  const capNote = list => list && list.length >= 500 ? ' <span class="dim" style="font-weight:400;font-size:12px">(first 500 shown)</span>' : '';
  return `
    <div class="card row">
      <span>${esc(d.root)} · ${(d.fileCount || 0).toLocaleString()} files · <b>${fmtBytes(d.total)}</b></span>
      <span style="color:var(--green)">cleanup potential: <b>${fmtBytes(d.cleanupPotential)}</b></span>
    </div>
    <div class="card"><h2 style="margin-top:0">Categories</h2>
      <table><thead><tr><th>Category</th><th class="num">Files</th><th class="num">Size</th></tr></thead><tbody>${cats}</tbody></table>
    </div>
    <div class="card"><h2 style="margin-top:0">Cleanup candidates <span class="dim" style="font-weight:400;font-size:12px">installers · archives · disk images · old files</span>${capNote(d.cleanupFiles)}</h2>
      <table><tbody>${cleanup || '<tr><td class="empty">nothing to clean</td></tr>'}</tbody></table>
    </div>
    ${large ? `<div class="card"><h2 style="margin-top:0">Large files${capNote(d.large)}</h2><table><tbody>${large}</tbody></table></div>` : ''}
    ${old ? `<div class="card"><h2 style="margin-top:0">Not modified recently${capNote(d.old)}</h2><table><tbody>${old}</tbody></table></div>` : ''}`;
};

function fileRow(f) {
  return `<tr data-path="${esc(f.path)}">
    <td>${f.db ? '<span class="badge db">db</span> ' : ''}<span class="mono" style="font-size:12px">${esc(f.path)}</span></td>
    <td class="num">${fmtBytes(f.size)}</td>
    <td class="dim">${fmtAgo(f.modTime)}</td>
    <td style="white-space:nowrap">${actionsHTML(f.path)}</td></tr>`;
}

// ---- router ----

const routes = {
  overview: screenOverview,
  folders: screenFolders,
  duplicates: screenDuplicates,
  deps: screenDeps,
  downloads: screenDownloads,
};

function route() {
  const name = (location.hash.replace(/^#\/?/, '') || 'overview').split('?')[0];
  stopPoll();
  document.querySelectorAll('#nav a[data-route]').forEach(a =>
    a.classList.toggle('active', a.dataset.route === name));
  (routes[name] || screenOverview)();
}

window.addEventListener('hashchange', route);
window.addEventListener('DOMContentLoaded', () => { loadHeader(); route(); });
