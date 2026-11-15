'use strict';

// Autocomplete for the query editor. It suggests SQL keywords, snippets,
// tables and columns (Postgres) or collections, methods and operators
// (MongoDB). Tab accepts (Enter too, after picking with
// the arrows), arrows move, Esc closes, Ctrl+Space opens
// the list on demand. It runs after app.js and uses its globals (state, api,
// el, editor, dbSelect).

const completer = (() => {
  const SQL_KEYWORDS = [
    'select', 'from', 'where', 'and', 'or', 'not', 'in', 'is null', 'is not null', 'like', 'ilike',
    'between', 'exists', 'join', 'left join', 'right join', 'inner join', 'full join', 'cross join',
    'on', 'using', 'group by', 'order by', 'having', 'limit', 'offset', 'distinct', 'as', 'asc', 'desc',
    'nulls first', 'nulls last', 'union', 'union all', 'except', 'intersect', 'with', 'case', 'when',
    'then', 'else', 'end', 'insert into', 'values', 'returning', 'update', 'set', 'delete from',
    'on conflict', 'do nothing', 'do update set', 'begin', 'commit', 'rollback', 'create table',
    'create index', 'alter table', 'add column', 'drop column', 'drop table', 'truncate', 'explain',
    'analyze', 'true', 'false', 'null', 'now()', 'count(*)', 'coalesce(', 'sum(', 'avg(', 'min(',
    'max(', 'array_agg(', 'string_agg(', 'json_agg(', 'jsonb_build_object(', 'date_trunc(',
    'extract(', 'cast(', 'lower(', 'upper(', 'length(', 'over (', 'partition by', 'filter (where',
  ];

  // Snippets end with a table position, so the list reopens with tables.
  const SQL_SNIPPETS = [
    { label: 'sel', text: 'select * from ', detail: 'select * from …' },
    { label: 'selc', text: 'select count(*) from ', detail: 'select count(*) from …' },
    { label: 'seld', text: 'select distinct ', detail: 'select distinct …' },
    { label: 'ins', text: 'insert into ', detail: 'insert into …' },
    { label: 'upd', text: 'update ', detail: 'update … set' },
    { label: 'del', text: 'delete from ', detail: 'delete from …' },
  ];

  const MONGO_METHODS = [
    'find({})', 'findOne({})', 'aggregate([])', 'countDocuments({})', 'estimatedDocumentCount()',
    'distinct("")', 'insertOne({})', 'insertMany([])', 'updateOne({}, { $set: {} })',
    'updateMany({}, { $set: {} })', 'replaceOne({}, {})', 'deleteOne({})', 'deleteMany({})',
    'createIndex({})', 'dropIndex("")', 'getIndexes()', 'drop()',
  ];
  const MONGO_CURSOR = ['sort({})', 'limit()', 'skip()', 'project({})', 'explain()'];
  const MONGO_OPERATORS = [
    '$eq', '$ne', '$gt', '$gte', '$lt', '$lte', '$in', '$nin', '$and', '$or', '$nor', '$not',
    '$exists', '$type', '$regex', '$elemMatch', '$size', '$all', '$expr', '$text', '$near',
    '$geoWithin', '$geoIntersects', '$set', '$unset', '$inc', '$push', '$pull', '$addToSet',
    '$match', '$group', '$project', '$sort', '$limit', '$skip', '$lookup', '$unwind', '$count',
    '$addFields', '$facet', '$sum', '$avg', '$min', '$max', '$first', '$last',
  ];
  const MONGO_TOP = ['show dbs', 'show collections', 'use ', 'db.', 'db.getCollection("")', 'db.runCommand({})',
    'ObjectId("")', 'ISODate("")', 'NumberLong()', 'NumberDecimal("")'];

  const columnCache = new Map(); // `${connId}|${db}|${object}` -> Promise<[{name, type}]>

  const box = el('ul', { id: 'complete', role: 'listbox', hidden: true });
  document.body.append(box);
  let items = [];
  let index = 0;
  let from = 0; // start of the word being replaced
  let reqSeq = 0;
  let navigated = false; // Enter accepts only after moving with the arrows

  // ---------- context ----------

  // The statement around the cursor: text after the last ';' or blank line
  // before it, up to the next one.
  function statementAround(pos) {
    const v = editor.value;
    const before = v.slice(0, pos);
    const cut = Math.max(before.lastIndexOf(';'), ...[...before.matchAll(/\n[ \t]*\n/g)].map((m) => m.index));
    const start = cut < 0 ? 0 : cut + 1;
    const rest = v.slice(pos);
    const m = rest.match(/;|\n[ \t]*\n/);
    return v.slice(start, m ? pos + m.index : v.length);
  }

  function tree() {
    const t = state.active && state.trees[state.active.id];
    return t && t.nodes ? t.nodes : null;
  }

  // Loads the object tree once, so tables can be suggested before the
  // connection was expanded in the sidebar.
  async function ensureTree() {
    const conn = state.active;
    if (!conn) return;
    if (!state.trees[conn.id]) await loadTree(conn);
    for (let i = 0; i < 50 && state.trees[conn.id] && state.trees[conn.id].loading; i++) {
      await new Promise((r) => setTimeout(r, 100));
    }
  }

  // Tables named after from/join/update/into in the statement, with aliases.
  function referencedTables(stmt) {
    const out = [];
    const re = /\b(?:from|join|update|into)\s+((?:"[^"]+"|[\w$]+)(?:\.(?:"[^"]+"|[\w$]+))?)(?:\s+(?:as\s+)?([a-z_][\w$]*))?/gi;
    const notAlias = /^(where|on|using|join|left|right|inner|full|cross|natural|group|order|limit|offset|set|values|select|returning|as|lateral|union|except|intersect|having|window|for)$/i;
    for (const m of stmt.matchAll(re)) {
      const alias = m[2] && !notAlias.test(m[2]) ? m[2] : null;
      out.push({ object: m[1], alias });
    }
    return out;
  }

  function columnsFor(object, database = '') {
    const conn = state.active;
    const key = `${conn.id}|${database}|${object}`;
    if (!columnCache.has(key)) {
      const params = new URLSearchParams({ object, database });
      const p = api(`/api/connections/${conn.id}/describe?${params}`)
        .then((d) => ((d.results && d.results[0] && d.results[0].rows) || []).map((r) => ({ name: String(r[0]), type: r[1] ? String(r[1]) : '' })))
        .catch(() => { columnCache.delete(key); return []; });
      columnCache.set(key, p);
    }
    return columnCache.get(key);
  }

  function tableItems() {
    const nodes = tree() || [];
    const out = [];
    for (const schema of nodes) {
      for (const t of schema.children || []) {
        // public is on the search path, so its tables need no schema.
        const label = schema.name === 'public' ? t.object.replace(/^public\./, '') : t.object;
        out.push({ label, detail: `${t.kind} · ${schema.name}`, kind: t.kind });
      }
    }
    return out;
  }

  // ---------- suggestions ----------

  async function sqlSuggestions(word, pos) {
    const v = editor.value;
    const stmt = statementAround(pos);
    const before = v.slice(0, pos - word.length);
    const upper = word && word === word.toUpperCase() && /[A-Z]/.test(word);
    const kw = (s) => (upper ? s.toUpperCase() : s);

    // qualifier.partial -> columns of that table or alias, or tables of a schema
    const dot = word.lastIndexOf('.');
    if (dot >= 0) {
      const qual = word.slice(0, dot).replace(/"/g, '');
      const refs = referencedTables(stmt);
      const ref = refs.find((r) => r.alias && r.alias.toLowerCase() === qual.toLowerCase())
        || refs.find((r) => r.object.replace(/"/g, '').split('.').pop().toLowerCase() === qual.toLowerCase());
      const out = [];
      if (ref) {
        for (const c of await columnsFor(ref.object)) out.push({ label: `${word.slice(0, dot + 1)}${c.name}`, detail: c.type, kind: 'column' });
      }
      const schema = (tree() || []).find((s) => s.name.toLowerCase() === qual.toLowerCase());
      if (schema) for (const t of schema.children || []) out.push({ label: `${word.slice(0, dot + 1)}${t.name}`, detail: t.kind, kind: t.kind });
      return out;
    }

    const prev = (before.match(/([\w$]+)\s*$/) || [])[1] || '';
    const tablePos = /^(from|join|update|into|table)$/i.test(prev);
    if (tablePos) {
      await ensureTree();
      return tableItems();
    }

    const out = [];
    if (word) {
      for (const s of SQL_SNIPPETS) if (s.label.startsWith(word.toLowerCase())) out.push({ label: s.label, detail: kw(s.detail), text: kw(s.text), kind: 'snippet', reopen: true });
    }
    const cols = [];
    for (const ref of referencedTables(stmt)) {
      for (const c of await columnsFor(ref.object)) cols.push({ label: c.name, detail: c.type, kind: 'column' });
    }
    out.push(...cols);
    for (const k of SQL_KEYWORDS) out.push({ label: kw(k), kind: 'keyword' });
    if (word) {
      await ensureTree();
      out.push(...tableItems());
    }
    return out;
  }

  async function mongoSuggestions(word) {
    const database = dbSelect.value;
    const colls = () => {
      const nodes = tree() || [];
      const dbNode = nodes.find((n) => n.name === database);
      if (dbNode) return (dbNode.children || []).map((c) => c.name);
      // No database picked: offer collections of every non-system database.
      const user = nodes.filter((n) => !['admin', 'config', 'local'].includes(n.name));
      return [...new Set(user.flatMap((n) => (n.children || []).map((c) => c.name)))];
    };
    if (word.startsWith('$')) return MONGO_OPERATORS.map((o) => ({ label: o, kind: 'operator' }));

    const parts = word.split('.');
    if (parts[0] === 'db' && parts.length === 2) {
      await ensureTree();
      return colls().map((c) => ({
        label: /^[A-Za-z_$][\w$]*$/.test(c) ? `db.${c}` : `db.getCollection(${JSON.stringify(c)})`,
        detail: 'collection', kind: 'collection', reopen: /^[A-Za-z_$][\w$]*$/.test(c), suffix: '.',
      }));
    }
    if (parts[0] === 'db' && parts.length === 3) {
      const base = `${parts[0]}.${parts[1]}.`;
      return MONGO_METHODS.map((m) => ({ label: base + m, kind: 'method', caretBack: /\(\{\}\)$|\(\[\]\)$|\(""\)$/.test(m) ? 2 : 0 }));
    }
    // After a closing paren: cursor modifiers.
    const v = editor.value;
    if (/\)\.$/.test(v.slice(0, editor.selectionStart - word.length + (word.startsWith('.') ? 1 : 0)))) {
      return MONGO_CURSOR.map((m) => ({ label: '.' + m, kind: 'method' }));
    }
    // Field names of the collection in the statement.
    const out = MONGO_TOP.map((t) => ({ label: t, kind: 'keyword', reopen: t === 'db.' }));
    const m = statementAround(editor.selectionStart).match(/db\.(?:getCollection\(\s*["']([^"']+)["']\s*\)|([A-Za-z_$][\w$]*))/);
    if (m) {
      for (const f of await columnsFor(m[1] || m[2], database)) out.unshift({ label: f.name, detail: f.type, kind: 'field' });
    }
    return out;
  }

  function rank(list, word) {
    const w = word.toLowerCase();
    const seen = new Set();
    const scored = [];
    for (const it of list) {
      const key = it.label + '|' + (it.text || '');
      if (seen.has(key)) continue;
      seen.add(key);
      const l = it.label.toLowerCase();
      const tail = l.split('.').pop();
      const wt = w.split('.').pop();
      let score;
      if (!w) score = 3;
      else if (l === w) score = it.text ? -1 : 4; // an exact snippet name wins
      else if (l.startsWith(w)) score = 0;
      else if (tail.startsWith(wt)) score = 1;
      else if (wt && tail.includes(wt)) score = 2;
      else continue;
      scored.push([score, it]);
    }
    scored.sort((a, b) => a[0] - b[0]);
    return scored.slice(0, 60).map((s) => s[1]).filter((it) => it.label.toLowerCase() !== w || it.text);
  }

  // ---------- popup ----------

  function currentWord() {
    const pos = editor.selectionStart;
    const before = editor.value.slice(0, pos);
    const word = (before.match(/(?:"[^"\n]*"\.)?[\w$."]*$/) || [''])[0];
    return { word, pos };
  }

  async function update(force) {
    if (!state.active || editor.selectionStart !== editor.selectionEnd) return close();
    const { word, pos } = currentWord();
    const before = editor.value.slice(0, pos - word.length);
    const prev = (before.match(/([\w$]+)\s*$/) || [])[1] || '';
    const afterTableKeyword = /^(from|join|update|into|table)$/i.test(prev) && /\s$/.test(before);
    if (!force && !word && !afterTableKeyword) return close();
    if (!force && /^\d/.test(word)) return close();
    // Not inside a string literal.
    const line = before.slice(before.lastIndexOf('\n') + 1);
    if (state.active.kind === 'redis') return close(); // commands, nothing to complete yet
    if (state.active.kind !== 'mongo' && (line.split("'").length - 1) % 2 === 1) return close();

    const ticket = ++reqSeq;
    const list = state.active.kind === 'mongo' ? await mongoSuggestions(word) : await sqlSuggestions(word, pos);
    if (ticket !== reqSeq) return;
    items = rank(list, word);
    from = pos - word.length;
    index = 0;
    navigated = false;
    if (!items.length) return close();
    render();
  }

  const kindTag = { keyword: 'kw', snippet: 'sn', column: 'col', field: 'fld', table: 'tbl', view: 'view', matview: 'mv', foreign: 'ft', collection: 'coll', method: 'fn', operator: 'op' };

  function render() {
    box.replaceChildren(...items.map((it, i) => el('li', {
      role: 'option',
      class: i === index ? 'on' : null,
      'aria-selected': i === index,
      onmousedown: (e) => { e.preventDefault(); index = i; accept(); },
    },
    el('span', { class: `ck ck-${it.kind}` }, kindTag[it.kind] || it.kind || ''),
    el('span', { class: 'cl' }, it.label),
    it.detail ? el('span', { class: 'cd' }, it.detail) : null)));
    box.hidden = false;
    place();
    const on = box.children[index];
    if (on) on.scrollIntoView({ block: 'nearest' });
  }

  function close() {
    reqSeq++;
    box.hidden = true;
    items = [];
  }

  function accept() {
    const it = items[index];
    if (!it) return close();
    const text = (it.text || it.label) + (it.suffix || '');
    const end = editor.selectionStart;
    editor.setRangeText(text, from, end, 'end');
    if (it.caretBack) editor.selectionStart = editor.selectionEnd = editor.selectionEnd - it.caretBack;
    editor.dispatchEvent(new Event('input'));
    close();
    if (it.reopen || /\s$/.test(text)) update(false);
  }

  // Caret position in pixels, from a hidden copy of the textarea.
  const mirror = el('div', { 'aria-hidden': 'true', class: 'complete-mirror' });
  document.body.append(mirror);
  function caretXY() {
    const cs = getComputedStyle(editor);
    for (const p of ['font', 'lineHeight', 'paddingTop', 'paddingLeft', 'paddingRight', 'paddingBottom', 'borderTopWidth', 'borderLeftWidth', 'letterSpacing', 'tabSize', 'whiteSpace', 'wordWrap']) {
      mirror.style[p] = cs[p];
    }
    mirror.style.width = editor.clientWidth + 'px';
    mirror.textContent = editor.value.slice(0, editor.selectionStart);
    const mark = el('span', {}, '​');
    mirror.append(mark);
    const r = editor.getBoundingClientRect();
    return {
      x: r.left + mark.offsetLeft - editor.scrollLeft,
      y: r.top + mark.offsetTop - editor.scrollTop,
      lh: parseFloat(cs.lineHeight) || 20,
    };
  }

  function place() {
    const { x, y, lh } = caretXY();
    const w = box.offsetWidth;
    const h = box.offsetHeight;
    let left = Math.min(x, window.innerWidth - w - 8);
    let top = y + lh;
    if (top + h > window.innerHeight - 8) top = Math.max(8, y - h - 2);
    box.style.left = Math.max(8, left) + 'px';
    box.style.top = top + 'px';
  }

  // ---------- wiring ----------

  let inputTimer;
  editor.addEventListener('input', (e) => {
    if (!e.isTrusted) return;
    clearTimeout(inputTimer);
    inputTimer = setTimeout(() => update(false), 60);
  });
  editor.addEventListener('blur', () => setTimeout(close, 100));
  editor.addEventListener('click', close);
  editor.addEventListener('scroll', () => { if (!box.hidden) place(); });

  // Called from app.js's keydown handler; returns true when it used the key.
  function onKeydown(e) {
    if (e.key === ' ' && e.ctrlKey) {
      e.preventDefault();
      update(true);
      return true;
    }
    if (box.hidden) return false;
    switch (e.key) {
      case 'ArrowDown': index = (index + 1) % items.length; navigated = true; render(); break;
      case 'ArrowUp': index = (index - 1 + items.length) % items.length; navigated = true; render(); break;
      case 'Tab':
        if (e.metaKey || e.ctrlKey || e.shiftKey) { close(); return false; }
        accept();
        break;
      case 'Enter':
        if (!navigated || e.metaKey || e.ctrlKey || e.shiftKey) { close(); return false; }
        accept();
        break;
      case 'Escape': close(); break;
      case 'ArrowLeft': case 'ArrowRight': case 'Home': case 'End': close(); return false;
      default: return false;
    }
    e.preventDefault();
    return true;
  }

  return { onKeydown, close, reset: () => columnCache.clear() };
})();
