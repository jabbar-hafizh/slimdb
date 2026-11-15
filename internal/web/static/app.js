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
  qtabs: [],            // query tabs: { id, connId, db, text, results, ... }
  qtab: null,           // id of the active query tab
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
  if (!state.qtabs.length) loadQTabs();
  // Tabs of deleted connections go away.
  state.qtabs = state.qtabs.filter((t) => state.conns.some((c) => c.id === t.connId));
  const want = state.qtabs.find((t) => t.id === state.qtab) || state.qtabs[state.qtabs.length - 1];
  if (want) activateQTab(want.id, { force: true });
  else showNoTab();
  renderTree();
}

function setActive(conn) {
  state.active = conn;
  $('#activeConn').textContent = conn ? conn.name : 'No connection selected';
  $('#activeConn').title = conn ? `${conn.kind}${conn.readOnly ? ', read-only' : ''}` : '';
  for (const b of ['#runBtn', '#runAllBtn', '#explainBtn', '#historyBtn']) $(b).disabled = !conn;
  const canDump = conn && (conn.kind === 'postgres' || conn.kind === 'mongo');
  $('#copyBtn').disabled = !canDump;
  $('#copyBtn').title = conn && !canDump ? 'Dump and import work for Postgres and MongoDB' : 'Copy this database into another connection, dump it to a file, or import a dump';
  editor.disabled = !conn;
  dbSelect.hidden = !conn || !hasDbSelect(conn);
  dbSelect.title = conn && conn.kind === 'redis' ? 'Redis database' : 'MongoDB database';
}

// selectConnection shows a query tab of conn, opening one when it has none.
function selectConnection(conn, { expand = true } = {}) {
  const cur = activeQTab();
  if (!cur || cur.connId !== conn.id) {
    const mine = state.qtabs.filter((t) => t.connId === conn.id);
    const pick = mine.sort((a, b) => (b.used || 0) - (a.used || 0))[0];
    if (pick) activateQTab(pick.id);
    else newQTab(conn);
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
  if (!conn || !hasDbSelect(conn)) return;
  const tree = state.trees[conn.id];
  const names = tree && tree.nodes ? tree.nodes.map((n) => n.name) : [];
  const tab = activeQTab();
  const current = tab ? tab.db || '' : '';
  dbSelect.replaceChildren(
    el('option', { value: '' }, '(database from URL)'),
    ...names.map((n) => el('option', { value: n }, n)),
  );
  if (current && !names.includes(current)) dbSelect.append(el('option', { value: current }, current));
  dbSelect.value = current;
}

// MongoDB and Redis pick their database per tab; SQL names it in the query.
function hasDbSelect(conn) {
  return conn.kind === 'mongo' || conn.kind === 'redis';
}

function setDatabase(name) {
  const tab = activeQTab();
  if (!tab) return;
  tab.db = name;
  saveQTabs();
  fillDbSelect();
  renderQTabs();
}

const kindLetter = { postgres: 'P', mongo: 'M', mysql: 'My', sqlite: 'Li', sqlserver: 'Ms', redis: 'R', key: 'K', string: 'K', hash: 'H', list: 'L', set: 'S', zset: 'Z', stream: 'X', table: 'T', view: 'V', matview: 'M', foreign: 'F', collection: 'C', schema: 'S', database: 'D' };

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
      if (hasDbSelect(conn) && state.active && state.active.id === conn.id) setDatabase(group.name);
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
        if (conn.kind === 'redis') setDatabase(group.name);
        describe(conn, child);
        renderTree();
      },
      ondblclick: () => insertQueryFor(conn, child),
    },
    kindIcon(child.kind),
    el('span', { class: 'label' }, child.name)));
  }
}

const redisRead = {
  string: (k) => `GET ${k}`,
  hash: (k) => `HGETALL ${k}`,
  list: (k) => `LRANGE ${k} 0 99`,
  set: (k) => `SMEMBERS ${k}`,
  zset: (k) => `ZRANGE ${k} 0 99 WITHSCORES`,
  stream: (k) => `XRANGE ${k} - + COUNT 100`,
};

function insertQueryFor(conn, obj) {
  let q;
  switch (conn.kind) {
    case 'mongo': {
      const ref = /^[A-Za-z_$][\w$]*$/.test(obj.name) ? `db.${obj.name}` : `db.getCollection(${JSON.stringify(obj.name)})`;
      q = `${ref}.find({})`;
      break;
    }
    case 'redis': {
      const key = /^[^\s"'\\]+$/.test(obj.name) ? obj.name : JSON.stringify(obj.name);
      q = (redisRead[obj.kind] || ((k) => `TYPE ${k}`))(key);
      break;
    }
    case 'sqlserver':
      q = `select top 100 * from ${obj.object};`;
      break;
    default:
      q = `select * from ${obj.object} limit 100;`;
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
  const tab = activeQTab();
  if (!tab) return;
  tab.text = editor.value;
  saveQTabs();
}

// ---------- query tabs ----------

// Each tab is its own console: a connection, a database (MongoDB), the
// editor text and the last results. Text and connection survive reloads;
// results stay in memory.

function loadQTabs() {
  const saved = storage.get('qtabs', null);
  if (Array.isArray(saved)) {
    state.qtabs = saved.map((t) => ({ ...t, results: [], rtab: 0, sort: null }));
    state.qtab = storage.get('qtab', null);
    return;
  }
  // First run with tabs: turn the old one-console-per-connection into tabs.
  const last = storage.get('activeConn', null);
  state.qtabs = state.conns
    .filter((c) => c.id === last || storage.get('editor.' + c.id, '').trim())
    .map((c) => ({ id: tabId(), connId: c.id, db: storage.get('db.' + c.id, ''), text: storage.get('editor.' + c.id, ''), results: [], rtab: 0, sort: null, used: c.id === last ? 1 : 0 }));
  const lastTab = state.qtabs.find((t) => t.connId === last);
  state.qtab = lastTab ? lastTab.id : null;
}

function tabId() {
  return Math.random().toString(36).slice(2, 10);
}

function saveQTabs() {
  storage.set('qtabs', state.qtabs.map(({ id, connId, db, text, used }) => ({ id, connId, db, text, used })));
  storage.set('qtab', state.qtab);
}

function activeQTab() {
  return state.qtabs.find((t) => t.id === state.qtab) || null;
}

function newQTab(conn, text = '') {
  const cur = activeQTab();
  const tab = { id: tabId(), connId: conn.id, db: cur && cur.connId === conn.id ? cur.db : '', text, results: [], rtab: 0, sort: null };
  state.qtabs.push(tab);
  activateQTab(tab.id);
  editor.focus();
  return tab;
}

// stashResults keeps the visible results with the tab they belong to.
function stashResults() {
  const cur = activeQTab();
  if (!cur) return;
  cur.results = state.results;
  cur.rtab = state.tab;
  cur.sort = state.sort;
  cur.totalMs = state.totalMs;
}

function activateQTab(id, { force = false } = {}) {
  if (!force && id === state.qtab) return;
  const prev = activeQTab();
  if (prev && prev.id !== id) {
    prev.text = editor.value;
    stashResults();
  }
  const tab = state.qtabs.find((t) => t.id === id);
  if (!tab) return;
  const conn = state.conns.find((c) => c.id === tab.connId);
  if (!conn) return;
  state.qtab = id;
  tab.used = Date.now();
  setActive(conn);
  storage.set('activeConn', conn.id);
  if (typeof completer !== 'undefined') completer.close();
  editor.value = tab.text || '';
  fillDbSelect();
  state.results = tab.results || [];
  state.tab = tab.rtab || 0;
  state.sort = tab.sort || null;
  state.totalMs = tab.totalMs;
  renderResults();
  saveQTabs();
  renderQTabs();
  renderTree();
}

function closeQTab(id) {
  const i = state.qtabs.findIndex((t) => t.id === id);
  if (i < 0) return;
  const tab = state.qtabs[i];
  if (tab.text && tab.text.trim() && id === state.qtab) tab.text = editor.value;
  state.qtabs.splice(i, 1);
  if (id === state.qtab) {
    const next = state.qtabs[i] || state.qtabs[i - 1];
    state.qtab = null;
    if (next) activateQTab(next.id, { force: true });
    else showNoTab();
  }
  saveQTabs();
  renderQTabs();
  renderTree();
}

function showNoTab() {
  state.qtab = null;
  setActive(null);
  editor.value = '';
  state.results = [];
  renderResults();
  renderQTabs();
}

function qtabLabel(tab) {
  const conn = state.conns.find((c) => c.id === tab.connId);
  let label = conn ? conn.name : '?';
  if (tab.db) label += ' · ' + tab.db;
  const same = state.qtabs.filter((t) => t.connId === tab.connId);
  if (same.length > 1) label += ` (${same.indexOf(tab) + 1})`;
  return label;
}

function renderQTabs() {
  const bar = $('#qtabs');
  bar.replaceChildren(...state.qtabs.map((t) => {
    const conn = state.conns.find((c) => c.id === t.connId);
    const label = qtabLabel(t);
    return el('div', {
      class: 'qtab' + (t.id === state.qtab ? ' on' : ''), role: 'tab', title: label,
      onclick: () => activateQTab(t.id),
      onauxclick: (e) => { if (e.button === 1) closeQTab(t.id); },
    },
    conn ? kindIcon(conn.kind) : null,
    el('span', { class: 'label' }, label),
    t.running ? el('span', { class: 'spin', title: 'Running' }, '●') : null,
    el('button', { class: 'close', title: 'Close tab', onclick: (e) => { e.stopPropagation(); closeQTab(t.id); } }, '×'));
  }),
  el('button', {
    class: 'qtab-add', title: 'New query tab for this connection',
    disabled: !state.active,
    onclick: () => state.active && newQTab(state.active),
  }, '+'));
  const on = bar.querySelector('.qtab.on');
  if (on) on.scrollIntoView({ block: 'nearest', inline: 'nearest' });
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
  if (e.key === 'Delete' && !editingCell && selectedEditRef() && !['TEXTAREA', 'INPUT', 'SELECT'].includes(document.activeElement.tagName)) {
    e.preventDefault();
    deleteRow();
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

async function run(mode, again) {
  const tab = activeQTab();
  if (!state.active || !tab || state.running) return;
  const query = again || (mode === 'all' ? editor.value : currentText());
  if (!again && state.results.some((x) => pendingCount(x)) && !confirm('Discard the unsaved changes in the results?')) return;
  if (!query.trim()) {
    toast('Nothing to run');
    return;
  }
  const explain = mode === 'explain';
  const conn = state.active;
  const body = {
    query,
    database: conn.kind === 'mongo' || conn.kind === 'redis' ? tab.db || '' : '',
    limit: Number($('#limit').value),
    analyze: explain && $('#analyze').checked,
  };
  const ctrl = new AbortController();
  state.running = ctrl;
  tab.running = true;
  renderQTabs();
  setRunning(true);
  const started = performance.now();
  const ticket = ++state.seq;
  let results;
  try {
    const data = await api(`/api/connections/${conn.id}/${explain ? 'explain' : 'query'}`, { method: 'POST', body, signal: ctrl.signal });
    results = data.results;
    if (!explain) tab.lastQuery = query;
  } catch (e) {
    results = [{ error: e.name === 'AbortError' ? 'Cancelled' : e.message, statement: query }];
  } finally {
    state.running = null;
    tab.running = false;
    setRunning(false);
    renderQTabs();
  }
  if (ticket !== state.seq) return;
  const ms = Math.round(performance.now() - started);
  if (tab.id === state.qtab) {
    showResults(results, ms);
  } else {
    // The user moved to another tab meanwhile: keep the results for later.
    const firstError = results.findIndex((r) => r.error);
    Object.assign(tab, { results, rtab: firstError >= 0 ? firstError : results.length - 1, sort: null, totalMs: ms });
  }
}

function setRunning(on) {
  $('#cancelBtn').hidden = !on;
  for (const b of ['#runBtn', '#runAllBtn', '#explainBtn']) $(b).disabled = on || !state.active;
  $('#resultMeta').textContent = on ? 'Running…' : $('#resultMeta').textContent;
}

async function describe(conn, obj) {
  const params = new URLSearchParams({ object: obj.object, database: obj.database || '' });
  const tab = activeQTab();
  $('#resultMeta').textContent = 'Loading…';
  const ticket = ++state.seq;
  try {
    const data = await api(`/api/connections/${conn.id}/describe?${params}`);
    if (ticket === state.seq && activeQTab() === tab) showResults(data.results);
  } catch (e) {
    if (ticket === state.seq && activeQTab() === tab) showResults([{ error: e.message }]);
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
  stashResults();
  renderResults();
}

function renderResults() {
  const tabs = $('#resultTabs');
  tabs.replaceChildren(...state.results.map((r, i) => el('button', {
    class: (i === state.tab ? 'on' : '') + (r.error ? ' err' : ''),
    role: 'tab',
    title: r.statement || '',
    onclick: () => { state.tab = i; state.sort = null; stashResults(); renderResults(); },
  }, r.title || (state.results.length > 1 ? `#${i + 1}` : 'Result'))));

  const r = state.results[state.tab];
  const body = $('#resultBody');
  const meta = $('#resultMeta');
  selectedCell = null;
  const hasGrid = r && !r.error && r.columns && r.columns.length > 0;
  $('#viewToggle').hidden = !(hasGrid && r.docs && r.docs.length);
  $('#copyCsv').hidden = !hasGrid;
  $('#copyJson').hidden = !hasGrid;
  renderEditBar(hasGrid ? r : null);

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
  if (canEdit(r)) out.push(' · ', el('span', { class: 'hint-inline' }, `editable (${r.edit.object}): double-click a cell`));
  return out;
}

const numericTypes = new Set(['int2', 'int4', 'int8', 'float4', 'float8', 'numeric', 'oid', 'money',
  'int', 'integer', 'tinyint', 'smallint', 'mediumint', 'bigint', 'decimal', 'float', 'double', 'real',
  'unsigned int', 'unsigned bigint', 'unsigned tinyint', 'unsigned smallint', 'unsigned mediumint', 'smallmoney']);

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
  const editable = canEdit(r);
  const pend = editable ? pending(r) : null;

  const head = el('tr', {}, el('th', { class: 'rownum' }, '#'), ...r.columns.map((c, i) => el('th', {
    title: (c.type ? `${c.name} (${c.type})` : c.name) + (editable && !editableCol(r, i) ? ', read-only' : ''),
    onclick: () => {
      const dir = state.sort && state.sort.col === i ? -state.sort.dir : 1;
      state.sort = { col: i, dir };
      stashResults();
      renderResults();
    },
  }, c.name,
  c.type ? el('span', { class: 'type' }, c.type) : null,
  editable && r.edit.key.includes(r.edit.columns[i]) ? el('span', { class: 'type', title: 'key' }, '🔑') : null,
  state.sort && state.sort.col === i ? el('span', { class: 'sort' }, state.sort.dir > 0 ? '▲' : '▼') : null)));

  const tbody = el('tbody');
  const addRow = (ref, label, row) => {
    const deleted = ref.ins === undefined && pend && pend.del[ref.row];
    const tr = el('tr', { class: [deleted ? 'deleted' : '', ref.ins !== undefined ? 'inserted' : ''].join(' ').trim() || null },
      el('td', { class: 'rownum' }, label));
    r.columns.forEach((col, ci) => {
      const change = pend && cellChange(pend, ref, ci);
      const v = change ? change.value : row[ci];
      const isNull = v === null || v === undefined;
      const numeric = typeof v === 'number' || numericTypes.has(col.type);
      const text = change ? (isNull ? 'NULL' : change.text) : (isNull ? 'NULL' : cellText(v));
      const td = el('td', {
        class: [isNull ? 'null' : '', numeric && !isNull ? 'num' : '', change ? 'dirty' : ''].join(' ').trim() || null,
        title: text.length > 60 ? text.slice(0, 500) : null,
      }, text.length > 300 ? text.slice(0, 300) + '…' : text);
      td.addEventListener('click', () => selectCell(td, v, { r, ref, ci }));
      td.addEventListener('dblclick', () => {
        if (editable && editableCol(r, ci) && !deleted) startEdit(td, r, ref, ci);
        else openCell(col.name, v);
      });
      tr.append(td);
    });
    tbody.append(tr);
  };
  for (const ri of order) addRow({ row: ri }, ri + 1, r.rows[ri]);
  if (pend) pend.ins.forEach((_, i) => addRow({ ins: i }, '+', r.columns.map(() => null)));
  return el('table', { class: 'grid' }, el('thead', {}, head), tbody);
}

// ---------- editing results ----------

// A result is editable when the driver says which table and key it comes
// from (r.edit) and the connection is not read-only. Changes wait in
// r.pending until Submit sends them in one batch.

function canEdit(r) {
  return !!(r && r.edit && state.active && !state.active.readOnly);
}

function isMongo() {
  return state.active && state.active.kind === 'mongo';
}

function editableCol(r, ci) {
  const name = r.edit.columns[ci];
  if (!name) return false;
  return !(isMongo() && name === '_id'); // MongoDB never changes an _id
}

function pending(r) {
  if (!r.pending) r.pending = { upd: {}, del: {}, ins: [] };
  return r.pending;
}

function pendingCount(r) {
  if (!r || !r.pending) return 0;
  const p = r.pending;
  return Object.keys(p.upd).filter((k) => !p.del[k]).length + Object.keys(p.del).length + p.ins.length;
}

function cellChange(p, ref, ci) {
  const row = ref.ins !== undefined ? p.ins[ref.ins] : p.upd[ref.row];
  return row && row[ci];
}

// original returns the cell's value as the editor shows it, plus its JSON
// type for MongoDB (strings are edited without quotes).
function originalFor(r, ref, ci) {
  if (ref.ins !== undefined) return { text: '', isNull: true, kind: 'none' };
  if (isMongo() && r.docs && r.docs[ref.row]) {
    let doc;
    try { doc = JSON.parse(r.docs[ref.row]); } catch { doc = {}; }
    const v = doc[r.columns[ci].name];
    if (v === undefined) return { text: '', isNull: true, kind: 'none' };
    if (v === null) return { text: '', isNull: true, kind: 'null' };
    if (typeof v === 'string') return { text: v, isNull: false, kind: 'string' };
    return { text: JSON.stringify(v), isNull: false, kind: 'json' };
  }
  const v = r.rows[ref.row][ci];
  return { text: v === null || v === undefined ? '' : cellText(v), isNull: v === null || v === undefined, kind: 'text' };
}

// toWire turns what was typed into what the server expects: text for SQL,
// relaxed Extended JSON for MongoDB.
function toWire(text, orig) {
  if (!isMongo()) return text;
  if (orig.kind === 'string') return JSON.stringify(text);
  try {
    JSON.parse(text);
    return text;
  } catch {
    if (orig.kind === 'json') throw new Error('Not valid JSON. Quote text, e.g. "abc".');
    return JSON.stringify(text); // a new field: plain text means a string
  }
}

function setChange(r, ref, ci, text, isNull) {
  const p = pending(r);
  const orig = originalFor(r, ref, ci);
  const target = ref.ins !== undefined ? p.ins[ref.ins] : (p.upd[ref.row] = p.upd[ref.row] || {});
  if (ref.ins === undefined && isNull === orig.isNull && (isNull || text === orig.text)) {
    delete target[ci]; // back to the original value
    if (!Object.keys(target).length) delete p.upd[ref.row];
    return;
  }
  target[ci] = isNull ? { value: null, text: 'NULL', wire: null } : { value: text, text, wire: toWire(text, orig) };
}

let editingCell = null;

function startEdit(td, r, ref, ci) {
  if (editingCell) return;
  const p = pending(r);
  const change = cellChange(p, ref, ci);
  const orig = originalFor(r, ref, ci);
  const start = change ? (change.wire === null ? '' : change.text) : orig.text;
  if (start.length > 200 || start.includes('\n')) {
    openCellEditor(r, ref, ci, start);
    return;
  }
  const input = el('input', { class: 'cell-input', value: start, placeholder: 'NULL' });
  td.replaceChildren(input);
  td.classList.add('editing');
  editingCell = { td, input };
  input.focus();
  input.select();
  let done = false;
  const finish = (save, move) => {
    if (done) return;
    done = true;
    editingCell = null;
    if (save) {
      const wasNull = change ? change.wire === null : orig.isNull;
      // An empty input on a NULL cell stays NULL; use the NULL button to null a value.
      const isNull = input.value === '' && wasNull;
      try {
        setChange(r, ref, ci, input.value, isNull);
      } catch (e) {
        toast(e.message);
      }
    }
    renderResults();
    if (move) {
      const next = nextEditable(r, ci, move);
      const cell = next !== null && findCell(ref, next);
      if (cell) startEdit(cell, r, ref, next);
    }
  };
  input.addEventListener('keydown', (e) => {
    e.stopPropagation();
    if (e.key === 'Enter') { e.preventDefault(); finish(true); }
    if (e.key === 'Escape') { e.preventDefault(); finish(false); }
    if (e.key === 'Tab') { e.preventDefault(); finish(true, e.shiftKey ? -1 : 1); }
  });
  input.addEventListener('blur', () => finish(true));
}

function nextEditable(r, ci, dir) {
  for (let i = ci + dir; i >= 0 && i < r.columns.length; i += dir) if (editableCol(r, i)) return i;
  return null;
}

// findCell finds the grid cell of row ref and column ci after a re-render.
function findCell(ref, ci) {
  const rows = [...$('#resultBody tbody').children];
  const r = state.results[state.tab];
  let tr;
  if (ref.ins !== undefined) {
    tr = rows.filter((x) => x.classList.contains('inserted'))[ref.ins];
  } else {
    tr = rows.find((x) => x.firstChild.textContent === String(ref.row + 1));
  }
  return tr && r ? tr.children[ci + 1] : null;
}

function openCellEditor(r, ref, ci, text) {
  $('#cellTitle').textContent = r.columns[ci].name;
  const area = el('textarea', { class: 'cell-area', spellcheck: 'false' });
  area.value = text;
  $('#cellValue').replaceChildren(area);
  $('#cellCopy').onclick = () => copyText(area.value);
  $('#cellApply').hidden = false;
  $('#cellApply').onclick = () => {
    try {
      setChange(r, ref, ci, area.value, false);
      $('#cellDialog').close();
      renderResults();
    } catch (e) {
      toast(e.message);
    }
  };
  $('#cellDialog').showModal();
  area.focus();
}

function selectedEditRef() {
  const r = state.results[state.tab];
  if (!selectedCell || !selectedCell.at || selectedCell.at.r !== r || !canEdit(r)) return null;
  return selectedCell.at;
}

function addRow() {
  const r = state.results[state.tab];
  if (!canEdit(r)) return;
  const p = pending(r);
  p.ins.push({});
  renderResults();
  // Start after the key: keys are usually generated (serial, identity, _id).
  let ci = r.columns.findIndex((_, i) => editableCol(r, i) && !r.edit.key.includes(r.edit.columns[i]));
  if (ci < 0) ci = nextEditable(r, -1, 1);
  const cell = ci !== null && findCell({ ins: p.ins.length - 1 }, ci);
  if (cell) {
    cell.scrollIntoView({ block: 'nearest' });
    startEdit(cell, r, { ins: p.ins.length - 1 }, ci);
  }
}

function deleteRow() {
  const at = selectedEditRef();
  if (!at) { toast('Select a cell of the row first'); return; }
  const p = pending(at.r);
  if (at.ref.ins !== undefined) p.ins.splice(at.ref.ins, 1);
  else if (p.del[at.ref.row]) delete p.del[at.ref.row];
  else p.del[at.ref.row] = true;
  selectedCell = null;
  renderResults();
}

function setNull() {
  const at = selectedEditRef();
  if (!at || !editableCol(at.r, at.ci)) { toast('Select an editable cell first'); return; }
  setChange(at.r, at.ref, at.ci, '', true);
  renderResults();
}

function revertEdits() {
  const r = state.results[state.tab];
  if (r) delete r.pending;
  renderResults();
}

function rowKey(r, ri) {
  const key = {};
  for (const k of r.edit.key) {
    const ci = r.edit.columns.indexOf(k);
    if (isMongo()) {
      const doc = JSON.parse(r.docs[ri]);
      key[k] = JSON.stringify(doc[r.columns[ci].name]);
    } else {
      key[k] = r.rows[ri][ci];
    }
  }
  return key;
}

function changeValues(r, changes) {
  const values = {};
  for (const [ci, c] of Object.entries(changes || {})) values[r.edit.columns[Number(ci)]] = c.wire;
  return values;
}

async function submitEdits() {
  const r = state.results[state.tab];
  const tab = activeQTab();
  if (!canEdit(r) || !tab || !pendingCount(r)) return;
  const p = r.pending;
  const ops = [];
  for (const [ri, changes] of Object.entries(p.upd)) {
    if (!p.del[ri]) ops.push({ op: 'update', key: rowKey(r, Number(ri)), values: changeValues(r, changes) });
  }
  for (const ri of Object.keys(p.del)) ops.push({ op: 'delete', key: rowKey(r, Number(ri)) });
  for (const row of p.ins) {
    const values = changeValues(r, row);
    if (Object.keys(values).length) ops.push({ op: 'insert', values });
  }
  if (!ops.length) { revertEdits(); return; }
  const dels = Object.keys(p.del).length;
  if (dels && !confirm(`Delete ${dels} row${dels === 1 ? '' : 's'} from ${r.edit.object}?`)) return;
  $('#editSubmit').disabled = true;
  try {
    const res = await api(`/api/connections/${state.active.id}/edit`, {
      method: 'POST',
      body: { database: r.edit.database || tab.db || '', object: r.edit.object, ops },
    });
    toast(res.message || 'Saved');
    delete r.pending;
    if (tab.lastQuery) await run('block', tab.lastQuery);
    else renderResults();
  } catch (e) {
    $('#editSubmit').disabled = false;
    showEditError(e.message);
  }
}

function showEditError(msg) {
  const meta = $('#resultMeta');
  meta.replaceChildren(el('span', { class: 'err' }, 'Not saved: ' + msg));
}

function renderEditBar(r) {
  const bar = $('#editBar');
  const on = canEdit(r) && state.view !== 'json';
  bar.hidden = !on;
  if (!on) return;
  const n = pendingCount(r);
  $('#editSubmit').textContent = n ? `Submit (${n})` : 'Submit';
  $('#editSubmit').disabled = !n;
  $('#editRevert').disabled = !n;
}

$('#editAdd').addEventListener('click', addRow);
$('#editDelete').addEventListener('click', deleteRow);
$('#editNull').addEventListener('click', setNull);
$('#editRevert').addEventListener('click', revertEdits);
$('#editSubmit').addEventListener('click', submitEdits);

function selectCell(td, value, at) {
  if (selectedCell) selectedCell.td.classList.remove('sel');
  selectedCell = { td, value, at };
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
  $('#cellApply').hidden = true;
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
    if (state.active && state.active.id === saved.id) setActive(saved);
    selectConnection(saved);
    renderQTabs();
  } catch (err) {
    setConnStatus(err.message, 'err');
  }
});

$('#connDelete').addEventListener('click', async () => {
  if (!editing || !confirm(`Delete connection "${editing.name}"? Its query tabs are closed too.`)) return;
  await api(`/api/connections/${editing.id}`, { method: 'DELETE' });
  try { localStorage.removeItem('slimdb.editor.' + editing.id); } catch { /* ignore */ }
  for (const t of state.qtabs.filter((q) => q.connId === editing.id)) closeQTab(t.id);
  connDialog.close();
  await loadConnections();
});

// ---------- dump, import and copy a database ----------

const copyForm = $('#copyForm');
let copyJob = null; // id of the job the dialog shows
let copyTimer = null;
let copyAction = storage.get('copyAction', 'dump');

function isLocalURL(u) {
  const m = /^[a-z+]+:\/\/(?:[^@/]*@)?([^/?]*)/i.exec(u || '');
  const hosts = m && m[1] ? m[1].split(',') : ['localhost'];
  return hosts.every((h) => /^(localhost|127(\.\d+){3}|\[::1\]|host\.docker\.internal)(:\d+)?$/i.test(h));
}

function copyTarget() {
  if (copyAction === 'import') return state.active;
  return state.conns.find((c) => c.id === copyForm.target.value);
}

const actionText = {
  dump: { button: 'Download', intro: (src) => `Dump ${src.name} to a file. Your browser saves it in its download folder.` },
  import: { button: 'Import', intro: (src) => `Import a dump file into ${src.name}.` },
  copy: { button: 'Copy', intro: (src) => `Copy ${src.name} straight into another connection of the same kind.` },
};

function updateCopyForm() {
  const src = state.active;
  if (!src) return;
  for (const b of $('#copyActions').children) b.classList.toggle('on', b.dataset.action === copyAction);
  $('#copyIntro').textContent = actionText[copyAction].intro(src);
  $('#copyStart').textContent = actionText[copyAction].button;
  $('#copyTargetRow').hidden = copyAction !== 'copy';
  $('#copyFileRow').hidden = copyAction !== 'import';
  copyForm.file.accept = src.kind === 'mongo' ? '.gz' : '.dump,.sql,.backup';

  let blocked = '';
  if (copyAction === 'copy') {
    const targets = state.conns.filter((c) => c.kind === src.kind && c.id !== src.id && !c.readOnly);
    targets.sort((a, b) => isLocalURL(b.url) - isLocalURL(a.url)); // dev into local is the usual case
    const keep = copyForm.target.value;
    copyForm.target.replaceChildren(...targets.map((c) => el('option', { value: c.id },
      c.name + (isLocalURL(c.url) ? '  (local)' : ''))));
    if (targets.some((c) => c.id === keep)) copyForm.target.value = keep;
    $('#copyNoTarget').hidden = targets.length > 0;
    if (!targets.length) blocked = 'no target';
  } else {
    $('#copyNoTarget').hidden = true;
  }
  if (copyAction === 'import' && src.readOnly) blocked = `${src.name} is read-only, so nothing can be imported into it.`;

  const t = copyTarget();
  const writes = copyAction !== 'dump';
  const remote = writes && t && !isLocalURL(t.url);
  $('#copyConfirmRow').hidden = !remote;
  if (remote) {
    copyForm.confirm.placeholder = t.name;
    $('#copyConfirmText').textContent = `${t.name} is not on this machine. Type its name to overwrite it`;
  }
  const mode = copyForm.mode.value;
  const objs = src.kind === 'mongo' ? 'collections' : 'tables';
  let effect = '';
  if (writes && t) {
    effect = mode === 'data' ? `Rows of matching ${objs} in ${t.name} are deleted first.` : `Matching ${objs} in ${t.name} are dropped and recreated.`;
    if (copyAction === 'import' && src.kind === 'postgres') effect += ' A plain .sql file runs as it is, whatever the mode.';
  } else if (copyAction === 'dump') {
    effect = src.kind === 'mongo' ? 'A .mongo.gz file slimdb can import again.' : 'A pg_dump custom-format .dump file; pg_restore reads it too.';
  }
  $('#copyEffect').textContent = blocked && blocked !== 'no target' ? blocked : effect;
  $('#copySchemasRow').hidden = src.kind !== 'postgres';
  if (!copyTimer) $('#copyStart').disabled = !!blocked;
}

function setCopyRunning(on) {
  $('#copyCancel').hidden = !on;
  $('#copyStart').disabled = on;
  for (const f of copyForm.querySelectorAll('select, input, #copyActions button')) f.disabled = on;
}

$('#copyBtn').addEventListener('click', () => {
  const src = state.active;
  if (!src) return;
  if (copyJob && copyTimer) { $('#copyDialog').showModal(); return; } // a job is still running
  copyForm.confirm.value = '';
  copyForm.file.value = '';
  $('#copyLog').hidden = true;
  $('#copyLog').textContent = '';
  setCopyStatus('');
  copyJob = null;
  setCopyRunning(false);
  updateCopyForm();
  $('#copyDialog').showModal();
});

$('#copyActions').addEventListener('click', (e) => {
  const action = e.target.dataset.action;
  if (!action || copyTimer) return;
  copyAction = action;
  storage.set('copyAction', action);
  setCopyStatus('');
  $('#copyLog').hidden = true;
  updateCopyForm();
});
copyForm.target.addEventListener('change', updateCopyForm);
for (const r of copyForm.querySelectorAll('input[name=mode]')) r.addEventListener('change', updateCopyForm);

function setCopyStatus(text, kind) {
  const s = $('#copyStatus');
  s.textContent = text;
  s.className = 'status' + (kind ? ' ' + kind : '');
}

function dumpParams(extra = {}) {
  return new URLSearchParams({
    mode: copyForm.mode.value,
    schemas: state.active.kind === 'postgres' ? copyForm.schemas.value : '',
    ...extra,
  });
}

async function startDump() {
  const src = state.active;
  setCopyStatus('Checking the connection…');
  try {
    const { file } = await api(`/api/connections/${src.id}/dump/check?${dumpParams()}`);
    // A plain link, so the browser streams the file to disk itself.
    const a = el('a', { href: `/api/connections/${src.id}/dump?${dumpParams({ token })}`, download: file });
    document.body.append(a);
    a.click();
    a.remove();
    setCopyStatus(`Downloading ${file}. Watch your browser's downloads; big databases take a while.`, 'ok');
  } catch (err) {
    setCopyStatus(err.message, 'err');
  }
}

async function startImport() {
  const t = state.active;
  const file = copyForm.file.files[0];
  if (!file) { setCopyStatus('Pick a dump file first', 'err'); return; }
  const mode = copyForm.mode.value;
  const objs = t.kind === 'mongo' ? 'collections' : 'tables';
  const effect = mode === 'data' ? `Rows of matching ${objs} are deleted first.` : `Matching ${objs} are dropped and recreated.`;
  if (!confirm(`Import ${file.name} into ${t.name}? ${effect}`)) return;
  setCopyRunning(true);
  setCopyStatus(`Uploading ${file.name}…`);
  try {
    const res = await fetch(`/api/connections/${t.id}/import?${dumpParams({ confirm: copyForm.confirm.value, file: file.name })}`, {
      method: 'POST',
      headers: { 'X-Slimdb-Token': token, 'Content-Type': 'application/octet-stream' },
      body: file,
    });
    const job = await res.json().catch(() => ({ error: res.statusText }));
    if (!res.ok) throw new Error(job.error || res.statusText);
    followJob(job);
  } catch (err) {
    setCopyRunning(false);
    setCopyStatus(err.message, 'err');
  }
}

function followJob(job) {
  copyJob = job.id;
  setCopyRunning(true);
  $('#copyLog').hidden = false;
  showCopy(job);
  copyTimer = setInterval(pollCopy, 1000);
}

copyForm.addEventListener('submit', async (e) => {
  e.preventDefault();
  if (!state.active) return;
  if (copyAction === 'dump') return startDump();
  if (copyAction === 'import') return startImport();
  const t = copyTarget();
  if (!t) return;
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
        schemas: copyForm.schemas.value.split(',').map((x) => x.trim()).filter(Boolean),
        confirm: copyForm.confirm.value,
      },
    });
    followJob(job);
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
    setCopyStatus(`${copyAction === 'import' ? 'Importing' : 'Copying'}… ${secs}s`);
    return;
  }
  clearInterval(copyTimer);
  copyTimer = null;
  setCopyRunning(false);
  updateCopyForm();
  if (job.state === 'done') {
    setCopyStatus(`Done in ${secs}s${warn}.`, 'ok');
    const t = state.conns.find((c) => c.name === job.target);
    if (t) delete state.trees[t.id];
    renderTree();
  } else if (job.state === 'cancelled') {
    setCopyStatus('Stopped. The target may be half written.', 'err');
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
