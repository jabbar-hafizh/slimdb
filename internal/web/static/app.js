'use strict';

// ---------- small helpers ----------

const $ = (sel) => document.querySelector(sel);
const token = document.querySelector('meta[name="slimdb-token"]').content;

// el builds DOM nodes. Text always goes through text nodes, never innerHTML,
// because cell values come straight from the database.
function el(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === undefined || v === null || v === false) continue;
    if (k.startsWith('on')) node.addEventListener(k.slice(2), v);
    else if (k === 'class') node.className = v;
    else node.setAttribute(k, v === true ? '' : v);
  }
  for (const c of children.flat()) {
    if (c === null || c === undefined || c === false) continue;
    node.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return node;
}

const storage = {
  get(key, fallback) {
    try {
      const v = localStorage.getItem('slimdb.' + key);
      return v === null ? fallback : JSON.parse(v);
    } catch { return fallback; }
  },
  set(key, value) {
    try { localStorage.setItem('slimdb.' + key, JSON.stringify(value)); } catch { /* storage unavailable */ }
  },
};

async function api(path, { method = 'GET', body, signal } = {}) {
  const res = await fetch(path, {
    method,
    signal,
    headers: { 'X-Slimdb-Token': token, ...(body ? { 'Content-Type': 'application/json' } : {}) },
    body: body ? JSON.stringify(body) : undefined,
  });
  if (res.status === 204) return null;
  const data = await res.json().catch(() => ({ error: res.statusText }));
  if (!res.ok) throw new Error(data.error || res.statusText);
  return data;
}

let toastTimer;
function toast(msg) {
  const t = $('#toast');
  t.textContent = msg;
  t.classList.add('show');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => t.classList.remove('show'), 1800);
}

async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
    toast('Copied');
  } catch {
    toast('Copy failed');
  }
}

// ---------- state ----------

const state = {
  conns: [],
  active: null,         // the selected connection
  trees: {},            // conn id -> { nodes, error, loading }
  expanded: new Set(storage.get('expanded', [])),
  collapsed: new Set(storage.get('collapsed', [])), // connection folders the user closed
  activeObject: null,   // tree key of the highlighted table/collection
  results: [],
  tab: 0,
  view: 'grid',
  sort: null,           // { col, dir }
  running: null,        // AbortController of the running request
  seq: 0,               // only the newest query/describe may show its results
};

const editor = $('#editor');
const dbSelect = $('#dbSelect');

// ---------- connections and tree ----------

async function loadConnections() {
  state.conns = await api('/api/connections');
  const last = storage.get('activeConn', null);
  const keep = state.active && state.conns.find((c) => c.id === state.active.id);
  const pick = keep || state.conns.find((c) => c.id === last) || null;
  if (pick) selectConnection(pick, { expand: false });
  else setActive(null);
  renderTree();
}

function setActive(conn) {
  state.active = conn;
  $('#activeConn').textContent = conn ? conn.name : 'No connection selected';
  $('#activeConn').title = conn ? `${conn.kind}${conn.readOnly ? ', read-only' : ''}` : '';
  for (const b of ['#runBtn', '#runAllBtn', '#explainBtn', '#historyBtn', '#copyBtn']) $(b).disabled = !conn;
  dbSelect.hidden = !conn || conn.kind !== 'mongo';
}

function selectConnection(conn, { expand = true } = {}) {
  if (state.active && state.active.id !== conn.id) saveEditor();
  const switching = !state.active || state.active.id !== conn.id;
  setActive(conn);
  if (typeof completer !== 'undefined') completer.close();
  storage.set('activeConn', conn.id);
  if (switching) {
    editor.value = storage.get('editor.' + conn.id, '');
    fillDbSelect();
  }
  const key = 'c:' + conn.id;
  if (expand && !state.expanded.has(key)) toggle(key);
  if (state.expanded.has(key) && !state.trees[conn.id]) loadTree(conn);
  renderTree();
}

async function loadTree(conn) {
  if (typeof completer !== 'undefined') completer.reset();
  state.trees[conn.id] = { loading: true };
  renderTree();
  try {
    state.trees[conn.id] = { nodes: await api(`/api/connections/${conn.id}/tree`) };
  } catch (e) {
    state.trees[conn.id] = { error: e.message };
  }
  if (state.active && state.active.id === conn.id) fillDbSelect();
  renderTree();
}

function toggle(key) {
  if (state.expanded.has(key)) state.expanded.delete(key);
  else state.expanded.add(key);
  storage.set('expanded', [...state.expanded]);
}

function fillDbSelect() {
  const conn = state.active;
  if (!conn || conn.kind !== 'mongo') return;
  const tree = state.trees[conn.id];
  const names = tree && tree.nodes ? tree.nodes.map((n) => n.name) : [];
  const current = storage.get('db.' + conn.id, '');
  dbSelect.replaceChildren(
    el('option', { value: '' }, '(database from URL)'),
    ...names.map((n) => el('option', { value: n }, n)),
  );
  if (current && !names.includes(current)) dbSelect.append(el('option', { value: current }, current));
  dbSelect.value = current;
}

function setDatabase(name) {
  if (!state.active) return;
  storage.set('db.' + state.active.id, name);
  fillDbSelect();
}

const kindLetter = { postgres: 'P', mongo: 'M', table: 'T', view: 'V', matview: 'M', foreign: 'F', collection: 'C', schema: 'S', database: 'D' };

function kindIcon(kind) {
  return el('span', { class: `kind-icon k-${kind}`, 'aria-hidden': 'true' }, kindLetter[kind] || '?');
}

function renderTree() {
  const filter = $('#treeFilter').value.trim().toLowerCase();
  const root = $('#tree');
  root.replaceChildren();
  if (state.conns.length === 0) {
    root.append(el('div', { class: 'tree-msg' }, 'No connections yet. Click + to add one.'));
    return;
  }
  // A name like "li3/dev/db_bvt_v2" puts the connection in folder "li3".
  const folders = new Map();
  for (const conn of state.conns) {
    const slash = conn.name.indexOf('/');
    const folder = slash > 0 ? conn.name.slice(0, slash) : '';
    if (!folders.has(folder)) folders.set(folder, []);
    folders.get(folder).push(conn);
  }
  for (const [folder, conns] of folders) {
    if (!folder) {
      for (const conn of conns) renderConn(root, conn, filter, 0, conn.name);
      continue;
    }
    const key = 'f:' + folder;
    const open = !state.collapsed.has(key);
    root.append(el('div', {
      class: 'node folder', role: 'treeitem', 'aria-expanded': open,
      onclick: () => {
        if (open) state.collapsed.add(key); else state.collapsed.delete(key);
        storage.set('collapsed', [...state.collapsed]);
        renderTree();
      },
    },
    el('span', { class: 'twisty' }, open ? '▾' : '▸'),
    el('span', { class: 'label' }, folder),
    el('span', { class: 'badge' }, String(conns.length))));
    if (open) for (const conn of conns) renderConn(root, conn, filter, 14, conn.name.slice(folder.length + 1));
  }
}

function renderConn(root, conn, filter, indent, label) {
  const key = 'c:' + conn.id;
  const open = state.expanded.has(key);
  const pad = (n) => `padding-left:${n + indent}px`;
  root.append(el('div', {
    class: 'node conn' + (state.active && state.active.id === conn.id ? ' active' : ''),
    style: indent ? pad(6) : null,
    title: conn.name,
    role: 'treeitem',
    'aria-expanded': open,
    onclick: () => {
      const wasActive = state.active && state.active.id === conn.id;
      if (wasActive) {
        toggle(key);
        if (state.expanded.has(key) && !state.trees[conn.id]) loadTree(conn);
        renderTree();
      } else {
        selectConnection(conn);
      }
    },
  },
  el('span', { class: 'twisty' }, open ? '▾' : '▸'),
  kindIcon(conn.kind),
  el('span', { class: 'label' }, label),
  conn.readOnly ? el('span', { class: 'badge' }, 'RO') : null,
  el('button', {
    class: 'edit', title: 'Edit connection',
    onclick: (e) => { e.stopPropagation(); openConnDialog(conn); },
  }, '✎')));

  if (!open) return;
  const tree = state.trees[conn.id];
  if (!tree || tree.loading) {
    root.append(el('div', { class: 'tree-msg', style: pad(28) }, 'Loading…'));
    return;
  }
  if (tree.error) {
    root.append(el('div', { class: 'tree-msg err', style: pad(28) }, tree.error));
    root.append(el('div', { class: 'tree-msg', style: pad(28) },
      el('button', { onclick: () => loadTree(conn) }, 'Retry')));
    return;
  }
  root.append(el('div', { class: 'tree-msg', style: pad(28) },
    el('button', { onclick: () => loadTree(conn), title: 'Reload objects' }, '↻ refresh')));
  for (const group of tree.nodes) renderGroup(root, conn, group, filter, indent);
}

function renderGroup(root, conn, group, filter, indent = 0) {
  const key = `g:${conn.id}:${group.name}`;
  const children = (group.children || []).filter((c) => !filter || c.name.toLowerCase().includes(filter));
  if (filter && children.length === 0) return;
  const open = filter ? true : state.expanded.has(key);
  root.append(el('div', {
    class: 'node', style: `padding-left:${22 + indent}px`, role: 'treeitem', 'aria-expanded': open,
    onclick: () => {
      toggle(key);
      if (conn.kind === 'mongo' && state.active && state.active.id === conn.id) setDatabase(group.name);
      renderTree();
    },
  },
  el('span', { class: 'twisty' }, open ? '▾' : '▸'),
  kindIcon(group.kind),
  el('span', { class: 'label' }, group.name),
  el('span', { class: 'badge' }, String((group.children || []).length))));
  if (!open) return;

  for (const child of children) {
    const ckey = `o:${conn.id}:${group.name}:${child.name}`;
    root.append(el('div', {
      class: 'node' + (state.activeObject === ckey ? ' active' : ''),
      style: `padding-left:${40 + indent}px`, role: 'treeitem',
      title: 'Click to describe, double-click to query',
      onclick: () => {
        state.activeObject = ckey;
        if (!state.active || state.active.id !== conn.id) selectConnection(conn, { expand: false });
        if (conn.kind === 'mongo') setDatabase(child.database);
        describe(conn, child);
        renderTree();
      },
      ondblclick: () => insertQueryFor(conn, child),
    },
    kindIcon(child.kind),
    el('span', { class: 'label' }, child.name)));
  }
}

function insertQueryFor(conn, obj) {
  let q;
  if (conn.kind === 'postgres') {
    q = `select * from ${obj.object} limit 100;`;
  } else {
    const ref = /^[A-Za-z_$][\w$]*$/.test(obj.name) ? `db.${obj.name}` : `db.getCollection(${JSON.stringify(obj.name)})`;
    q = `${ref}.find({})`;
  }
  appendToEditor(q);
  run('block');
}

function appendToEditor(text) {
  const base = editor.value.replace(/\s+$/, '');
  editor.value = base ? `${base}\n\n${text}` : text;
  const end = editor.value.length;
  editor.focus();
  editor.setSelectionRange(end - text.length, end);
  saveEditor();
}

// ---------- editor ----------

let saveTimer;
function saveEditor() {
  if (state.active) storage.set('editor.' + state.active.id, editor.value);
}
editor.addEventListener('input', () => {
  clearTimeout(saveTimer);
  saveTimer = setTimeout(saveEditor, 300);
});

editor.addEventListener('keydown', (e) => {
  if (completer.onKeydown(e)) return;
  if (e.key === 'Tab' && !e.metaKey && !e.ctrlKey) {
    e.preventDefault();
    editor.setRangeText('  ', editor.selectionStart, editor.selectionEnd, 'end');
  }
});

document.addEventListener('keydown', (e) => {
  if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') {
    e.preventDefault();
    run(e.shiftKey ? 'all' : 'block');
  }
  if ((e.metaKey || e.ctrlKey) && e.key === 'c' && selectedCell && document.activeElement !== editor
      && !window.getSelection().toString()) {
    e.preventDefault();
    copyText(cellText(selectedCell.value));
  }
});

// currentText returns the selection, or the block around the cursor. Blocks
// are separated by blank lines, so a console can hold many queries.
function currentText() {
  const { selectionStart: s, selectionEnd: e, value } = editor;
  if (e > s) return value.slice(s, e);
  const before = value.slice(0, s);
  const after = value.slice(s);
  const startMatch = [...before.matchAll(/\n[ \t]*\n/g)].pop();
  const start = startMatch ? startMatch.index + startMatch[0].length : 0;
  const endMatch = after.match(/\n[ \t]*\n/);
  const end = endMatch ? s + endMatch.index : value.length;
  return value.slice(start, end);
}

// ---------- running ----------

async function run(mode) {
  if (!state.active || state.running) return;
  const query = mode === 'all' ? editor.value : currentText();
  if (!query.trim()) {
    toast('Nothing to run');
    return;
  }
  const explain = mode === 'explain';
  const body = {
    query,
    database: state.active.kind === 'mongo' ? dbSelect.value : '',
    limit: Number($('#limit').value),
    analyze: explain && $('#analyze').checked,
  };
  const ctrl = new AbortController();
  state.running = ctrl;
  setRunning(true);
  const started = performance.now();
  const ticket = ++state.seq;
  try {
    const data = await api(`/api/connections/${state.active.id}/${explain ? 'explain' : 'query'}`, { method: 'POST', body, signal: ctrl.signal });
    if (ticket === state.seq) showResults(data.results, Math.round(performance.now() - started));
  } catch (e) {
    if (ticket === state.seq) showResults([{ error: e.name === 'AbortError' ? 'Cancelled' : e.message, statement: query }], 0);
  } finally {
    state.running = null;
    setRunning(false);
  }
}

function setRunning(on) {
  $('#cancelBtn').hidden = !on;
  for (const b of ['#runBtn', '#runAllBtn', '#explainBtn']) $(b).disabled = on || !state.active;
  $('#resultMeta').textContent = on ? 'Running…' : $('#resultMeta').textContent;
}

async function describe(conn, obj) {
  const params = new URLSearchParams({ object: obj.object, database: obj.database || '' });
  $('#resultMeta').textContent = 'Loading…';
  const ticket = ++state.seq;
  try {
    const data = await api(`/api/connections/${conn.id}/describe?${params}`);
    if (ticket === state.seq) showResults(data.results);
  } catch (e) {
    if (ticket === state.seq) showResults([{ error: e.message }]);
  }
}

// ---------- results ----------

let selectedCell = null;

function showResults(results, totalMs) {
  state.results = results || [];
  state.totalMs = totalMs;
  const firstError = state.results.findIndex((r) => r.error);
  state.tab = firstError >= 0 ? firstError : state.results.length - 1;
  state.sort = null;
  renderResults();
}

function renderResults() {
  const tabs = $('#resultTabs');
  tabs.replaceChildren(...state.results.map((r, i) => el('button', {
    class: (i === state.tab ? 'on' : '') + (r.error ? ' err' : ''),
    role: 'tab',
    title: r.statement || '',
    onclick: () => { state.tab = i; state.sort = null; renderResults(); },
  }, r.title || (state.results.length > 1 ? `#${i + 1}` : 'Result'))));

  const r = state.results[state.tab];
  const body = $('#resultBody');
  const meta = $('#resultMeta');
  selectedCell = null;
  const hasGrid = r && !r.error && r.columns && r.columns.length > 0;
  $('#viewToggle').hidden = !(hasGrid && r.docs && r.docs.length);
  $('#copyCsv').hidden = !hasGrid;
  $('#copyJson').hidden = !hasGrid;

  if (!r) {
    meta.textContent = '';
    body.replaceChildren(el('div', { class: 'empty' }, 'Results show up here.'));
    return;
  }
  meta.replaceChildren(...metaLine(r));
  if (r.error) {
    body.replaceChildren(el('div', { class: 'error-box' }, r.error));
  } else if (!hasGrid) {
    body.replaceChildren(el('div', { class: 'msg-box' }, r.message || 'OK'));
  } else if (state.view === 'json' && r.docs && r.docs.length) {
    body.replaceChildren(el('pre', { class: 'json-view' }, r.docs.map(prettyJSON).join('\n\n')));
  } else {
    body.replaceChildren(gridTable(r));
  }
  body.scrollTop = 0;
}

function metaLine(r) {
  const parts = [];
  if (r.columns && r.columns.length) parts.push(`${r.rows.length} row${r.rows.length === 1 ? '' : 's'}`);
  else if (r.affected) parts.push(`${r.affected} affected`);
  if (r.message && !(r.columns && r.columns.length)) parts.push(r.message);
  if (typeof r.ms === 'number') parts.push(`${r.ms} ms`);
  const out = [parts.join(' · ')];
  if (r.truncated) out.push(' · ', el('span', { class: 'warn' }, 'more rows exist, raise the limit or refine the query'));
  return out;
}

const numericTypes = new Set(['int2', 'int4', 'int8', 'float4', 'float8', 'numeric', 'oid', 'money']);

function gridTable(r) {
  const order = r.rows.map((_, i) => i);
  if (state.sort) {
    const { col, dir } = state.sort;
    const numeric = numericTypes.has(r.columns[col].type) || r.rows.every((row) => row[col] === null || typeof row[col] === 'number');
    order.sort((a, b) => {
      const x = r.rows[a][col];
      const y = r.rows[b][col];
      if (x === null || x === undefined) return 1;
      if (y === null || y === undefined) return -1;
      const c = numeric ? Number(x) - Number(y) : cellText(x).localeCompare(cellText(y), undefined, { numeric: true });
      return dir * c;
    });
  }

  const head = el('tr', {}, el('th', { class: 'rownum' }, '#'), ...r.columns.map((c, i) => el('th', {
    title: c.type ? `${c.name} (${c.type})` : c.name,
    onclick: () => {
      const dir = state.sort && state.sort.col === i ? -state.sort.dir : 1;
      state.sort = { col: i, dir };
      renderResults();
    },
  }, c.name,
  c.type ? el('span', { class: 'type' }, c.type) : null,
  state.sort && state.sort.col === i ? el('span', { class: 'sort' }, state.sort.dir > 0 ? '▲' : '▼') : null)));

  const tbody = el('tbody');
  for (const ri of order) {
    const row = r.rows[ri];
    const tr = el('tr', {}, el('td', { class: 'rownum' }, ri + 1));
    row.forEach((v, ci) => {
      const isNull = v === null || v === undefined;
      const numeric = typeof v === 'number' || numericTypes.has(r.columns[ci].type);
      const text = isNull ? 'NULL' : cellText(v);
      const td = el('td', {
        class: [isNull ? 'null' : '', numeric && !isNull ? 'num' : ''].join(' ').trim() || null,
        title: text.length > 60 ? text.slice(0, 500) : null,
      }, text.length > 300 ? text.slice(0, 300) + '…' : text);
      td.addEventListener('click', () => selectCell(td, v));
      td.addEventListener('dblclick', () => openCell(r.columns[ci].name, v));
      tr.append(td);
    });
    tbody.append(tr);
  }
  return el('table', { class: 'grid' }, el('thead', {}, head), tbody);
}

function selectCell(td, value) {
  if (selectedCell) selectedCell.td.classList.remove('sel');
  selectedCell = { td, value };
  td.classList.add('sel');
}

function cellText(v) {
  if (v === null || v === undefined) return '';
  return typeof v === 'string' ? v : JSON.stringify(v);
}

function prettyJSON(s) {
  try { return JSON.stringify(JSON.parse(s), null, 2); } catch { return s; }
}

function openCell(name, value) {
  $('#cellTitle').textContent = name;
  const text = value === null || value === undefined ? 'NULL' : prettyJSON(cellText(value));
  $('#cellValue').textContent = text;
  $('#cellCopy').onclick = () => copyText(text);
  $('#cellDialog').showModal();
}

function resultAsCSV(r) {
  const esc = (v) => {
    const s = cellText(v);
    return /[",\n\r]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s;
  };
  return [r.columns.map((c) => esc(c.name)).join(','), ...r.rows.map((row) => row.map(esc).join(','))].join('\n');
}

function resultAsJSON(r) {
  if (r.docs && r.docs.length) return `[\n${r.docs.map((d) => '  ' + d).join(',\n')}\n]`;
  const objs = r.rows.map((row) => Object.fromEntries(r.columns.map((c, i) => [c.name, row[i]])));
  return JSON.stringify(objs, null, 2);
}

$('#copyCsv').addEventListener('click', () => copyText(resultAsCSV(state.results[state.tab])));
$('#copyJson').addEventListener('click', () => copyText(resultAsJSON(state.results[state.tab])));
$('#viewToggle').addEventListener('click', (e) => {
  const view = e.target.dataset.view;
  if (!view) return;
  state.view = view;
  for (const b of $('#viewToggle').children) b.classList.toggle('on', b.dataset.view === view);
  renderResults();
});

// ---------- connection dialog ----------

const connDialog = $('#connDialog');
const connForm = $('#connForm');
let editing = null;

function openConnDialog(conn) {
  editing = conn || null;
  $('#connTitle').textContent = conn ? 'Edit connection' : 'New connection';
  connForm.name.value = conn ? conn.name : '';
  connForm.url.value = conn ? conn.url : '';
  connForm.readOnly.checked = conn ? conn.readOnly : false;
  $('#connDelete').hidden = !conn;
  setConnStatus('');
  connDialog.showModal();
  connForm.name.focus();
}

function formConn() {
  return {
    id: editing ? editing.id : '',
    name: connForm.name.value,
    url: connForm.url.value,
    readOnly: connForm.readOnly.checked,
  };
}

function setConnStatus(text, kind) {
  const s = $('#connStatus');
  s.textContent = text;
  s.className = 'status' + (kind ? ' ' + kind : '');
}

$('#addConn').addEventListener('click', () => openConnDialog(null));
$('#connCancel').addEventListener('click', () => connDialog.close());

$('#connTest').addEventListener('click', async () => {
  setConnStatus('Connecting…');
  try {
    const r = await api('/api/connections/test', { method: 'POST', body: formConn() });
    setConnStatus(`Connected in ${r.ms} ms`, 'ok');
  } catch (e) {
    setConnStatus(e.message, 'err');
  }
});

connForm.addEventListener('submit', async (e) => {
  e.preventDefault();
  try {
    const saved = await api('/api/connections', { method: 'POST', body: formConn() });
    connDialog.close();
    delete state.trees[saved.id];
    state.conns = await api('/api/connections');
    selectConnection(saved);
  } catch (err) {
    setConnStatus(err.message, 'err');
  }
});

$('#connDelete').addEventListener('click', async () => {
  if (!editing || !confirm(`Delete connection "${editing.name}"? Its saved editor text is deleted too.`)) return;
  await api(`/api/connections/${editing.id}`, { method: 'DELETE' });
  try { localStorage.removeItem('slimdb.editor.' + editing.id); } catch { /* ignore */ }
  if (state.active && state.active.id === editing.id) {
    state.active = null;
    editor.value = '';
  }
  connDialog.close();
  await loadConnections();
});

// ---------- copy a database ----------

const copyForm = $('#copyForm');
let copyJob = null; // id of the job the dialog shows
let copyTimer = null;

function isLocalURL(u) {
  const m = /^[a-z+]+:\/\/(?:[^@/]*@)?([^/?]*)/i.exec(u || '');
  const hosts = m && m[1] ? m[1].split(',') : ['localhost'];
  return hosts.every((h) => /^(localhost|127(\.\d+){3}|\[::1\]|host\.docker\.internal)(:\d+)?$/i.test(h));
}

function copyTarget() {
  return state.conns.find((c) => c.id === copyForm.target.value);
}

function updateCopyForm() {
  const t = copyTarget();
  const remote = t && !isLocalURL(t.url);
  $('#copyConfirmRow').hidden = !remote;
  if (remote) copyForm.confirm.placeholder = t.name;
}

function setCopyRunning(on) {
  $('#copyCancel').hidden = !on;
  $('#copyStart').disabled = on;
  for (const f of copyForm.querySelectorAll('select, input')) f.disabled = on;
}

$('#copyBtn').addEventListener('click', () => {
  const src = state.active;
  if (!src) return;
  if (copyJob && copyTimer) { $('#copyDialog').showModal(); return; } // a copy is still running
  $('#copySource').textContent = src.name;
  const targets = state.conns.filter((c) => c.kind === src.kind && c.id !== src.id && !c.readOnly);
  // Local connections first: copying dev into local is the usual case.
  targets.sort((a, b) => isLocalURL(b.url) - isLocalURL(a.url));
  copyForm.target.replaceChildren(...targets.map((c) => el('option', { value: c.id },
    c.name + (isLocalURL(c.url) ? '  (local)' : ''))));
  $('#copyNoTarget').hidden = targets.length > 0;
  $('#copyStart').disabled = targets.length === 0;
  $('#copySchemasRow').hidden = src.kind !== 'postgres';
  copyForm.confirm.value = '';
  $('#copyLog').hidden = true;
  $('#copyLog').textContent = '';
  setCopyStatus('');
  copyJob = null;
  setCopyRunning(false);
  $('#copyStart').disabled = targets.length === 0;
  updateCopyForm();
  $('#copyDialog').showModal();
});

copyForm.target.addEventListener('change', updateCopyForm);

function setCopyStatus(text, kind) {
  const s = $('#copyStatus');
  s.textContent = text;
  s.className = 'status' + (kind ? ' ' + kind : '');
}

copyForm.addEventListener('submit', async (e) => {
  e.preventDefault();
  const t = copyTarget();
  if (!state.active || !t) return;
  const mode = copyForm.mode.value;
  const what = { all: 'everything', schema: 'the schema', data: 'the data' }[mode];
  const objs = t.kind === 'mongo' ? 'collections' : 'tables';
  const effect = mode === 'data' ? `Rows of matching ${objs} in ${t.name} are deleted first.` : `Matching ${objs} in ${t.name} are dropped and recreated.`;
  if (!confirm(`Copy ${what} of ${state.active.name} into ${t.name}? ${effect}`)) return;
  try {
    const job = await api('/api/copy', {
      method: 'POST',
      body: {
        source: state.active.id, target: t.id, mode,
        schemas: copyForm.schemas.value.split(',').map((s) => s.trim()).filter(Boolean),
        confirm: copyForm.confirm.value,
      },
    });
    copyJob = job.id;
    setCopyRunning(true);
    $('#copyLog').hidden = false;
    showCopy(job);
    copyTimer = setInterval(pollCopy, 1000);
  } catch (err) {
    setCopyStatus(err.message, 'err');
  }
});

async function pollCopy() {
  if (!copyJob) return;
  try {
    showCopy(await api(`/api/copy/${copyJob}`));
  } catch (err) {
    setCopyStatus(err.message, 'err');
  }
}

function showCopy(job) {
  const log = $('#copyLog');
  const atBottom = log.scrollTop + log.clientHeight >= log.scrollHeight - 20;
  log.textContent = (job.dropped ? `… ${job.dropped} earlier lines\n` : '') + job.log.join('\n');
  if (atBottom) log.scrollTop = log.scrollHeight;
  const secs = Math.round(((job.finished && !job.finished.startsWith('0001') ? new Date(job.finished) : new Date()) - new Date(job.started)) / 1000);
  const warn = job.warnings.length ? `, ${job.warnings.length} warning(s), see the log` : '';
  if (job.state === 'running') {
    setCopyStatus(`Copying… ${secs}s`);
    return;
  }
  clearInterval(copyTimer);
  copyTimer = null;
  setCopyRunning(false);
  if (job.state === 'done') {
    setCopyStatus(`Done in ${secs}s${warn}.`, 'ok');
    const t = state.conns.find((c) => c.name === job.target);
    if (t) delete state.trees[t.id];
    renderTree();
  } else if (job.state === 'cancelled') {
    setCopyStatus('Stopped. The target may be half copied.', 'err');
  } else {
    setCopyStatus(job.error, 'err');
  }
}

$('#copyCancel').addEventListener('click', () => {
  if (copyJob) api(`/api/copy/${copyJob}/cancel`, { method: 'POST' }).catch(() => {});
});

// ---------- history ----------

$('#historyBtn').addEventListener('click', async () => {
  if (!state.active) return;
  const list = $('#historyList');
  list.replaceChildren(el('div', { class: 'tree-msg' }, 'Loading…'));
  $('#historyDialog').showModal();
  try {
    const entries = await api(`/api/history?conn=${encodeURIComponent(state.active.id)}`);
    if (entries.length === 0) {
      list.replaceChildren(el('div', { class: 'tree-msg' }, 'No history for this connection yet.'));
      return;
    }
    list.replaceChildren(...entries.map((h) => el('div', {
      class: 'hist',
      onclick: () => { $('#historyDialog').close(); appendToEditor(h.query.trim()); },
    },
    el('div', { class: 'meta' },
      el('span', {}, new Date(h.at).toLocaleString()),
      el('span', {}, `${h.ms} ms`),
      el('span', {}, h.source),
      h.database ? el('span', {}, h.database) : null,
      h.error ? el('span', { class: 'err', title: h.error }, 'failed') : null),
    el('pre', {}, h.query))));
  } catch (e) {
    list.replaceChildren(el('div', { class: 'tree-msg err' }, e.message));
  }
});

// ---------- misc wiring ----------

for (const btn of document.querySelectorAll('[data-close]')) {
  btn.addEventListener('click', () => btn.closest('dialog').close());
}

$('#runBtn').addEventListener('click', () => run('block'));
$('#runAllBtn').addEventListener('click', () => run('all'));
$('#explainBtn').addEventListener('click', () => run('explain'));
$('#cancelBtn').addEventListener('click', () => state.running && state.running.abort());
$('#treeFilter').addEventListener('input', renderTree);
dbSelect.addEventListener('change', () => setDatabase(dbSelect.value));
$('#limit').value = String(storage.get('limit', 500));
$('#limit').addEventListener('change', () => storage.set('limit', Number($('#limit').value)));

// Drag the bar between the editor and the results.
(() => {
  const pane = $('#editorPane');
  const saved = storage.get('editorHeight', null);
  if (saved) pane.style.height = saved + 'px';
  $('#splitter').addEventListener('mousedown', (e) => {
    e.preventDefault();
    const startY = e.clientY;
    const startH = pane.getBoundingClientRect().height;
    const move = (ev) => {
      const h = Math.max(60, Math.min(window.innerHeight - 160, startH + ev.clientY - startY));
      pane.style.height = h + 'px';
    };
    const up = () => {
      document.removeEventListener('mousemove', move);
      document.removeEventListener('mouseup', up);
      storage.set('editorHeight', Math.round(pane.getBoundingClientRect().height));
    };
    document.addEventListener('mousemove', move);
    document.addEventListener('mouseup', up);
  });
})();

setActive(null);
loadConnections().catch((e) => {
  $('#tree').replaceChildren(el('div', { class: 'tree-msg err' }, e.message));
});
