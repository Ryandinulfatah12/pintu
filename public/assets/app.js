'use strict';

const $ = (sel, root = document) => root.querySelector(sel);
const DEFAULT_PORT = { rdp: 3389, ssh: 22 };

const state = {
  connections: [],
  status: new Map(),
  group: null, // null = all
  query: '',
  editing: null,
};

// ------------------------------------------------------------------ api
async function api(path, options = {}) {
  const res = await fetch('/api' + path, {
    ...options,
    headers: { 'Content-Type': 'application/json', ...(options.headers || {}) },
  });
  if (res.status === 401) { location.replace('/login'); throw new Error('unauthorized'); }
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || `Gagal (${res.status})`);
  return data;
}

function toast(msg) {
  const el = document.createElement('div');
  el.className = 'toast';
  el.setAttribute('role', 'status');
  el.textContent = msg;
  document.body.append(el);
  setTimeout(() => el.remove(), 2600);
}

// --------------------------------------------------------------- render
function groupsOf(list) {
  const map = new Map();
  for (const c of list) {
    const g = c.group || 'Tanpa grup';
    map.set(g, (map.get(g) || 0) + 1);
  }
  return [...map.entries()].sort((a, b) => a[0].localeCompare(b[0], 'id'));
}

function renderGroups() {
  const ul = $('#groups');
  ul.replaceChildren();
  const items = [[null, 'Semua', state.connections.length], ...groupsOf(state.connections).map(([g, n]) => [g, g, n])];
  if (state.group !== null && !items.some(([v]) => v === state.group)) state.group = null;
  for (const [value, label, count] of items) {
    const li = document.createElement('li');
    const btn = document.createElement('button');
    btn.type = 'button';
    btn.setAttribute('aria-current', String(state.group === value));
    btn.innerHTML = '<span></span><span class="count"></span>';
    btn.firstChild.textContent = label;
    btn.lastChild.textContent = count;
    btn.addEventListener('click', () => { state.group = value; render(); });
    li.append(btn);
    ul.append(li);
  }

  const dl = $('#group-list');
  dl.replaceChildren(...groupsOf(state.connections)
    .filter(([g]) => g !== 'Tanpa grup')
    .map(([g]) => Object.assign(document.createElement('option'), { value: g })));
}

function visible() {
  const q = state.query.toLowerCase();
  return state.connections.filter((c) => {
    if (state.group !== null && (c.group || 'Tanpa grup') !== state.group) return false;
    if (!q) return true;
    return [c.name, c.hostname, c.username, c.group, c.notes].some((v) => v && v.toLowerCase().includes(q));
  });
}

function relTime(sqlDate) {
  if (!sqlDate) return 'Belum pernah dibuka';
  const d = new Date(sqlDate.replace(' ', 'T') + 'Z');
  const mins = Math.round((Date.now() - d) / 60000);
  if (mins < 1) return 'Dibuka barusan';
  if (mins < 60) return `Dibuka ${mins} menit lalu`;
  const h = Math.round(mins / 60);
  if (h < 24) return `Dibuka ${h} jam lalu`;
  return `Dibuka ${d.toLocaleDateString('id-ID', { day: 'numeric', month: 'short' })}`;
}

function applyStatus(li, id) {
  const s = state.status.get(id);
  const reach = $('.reach', li);
  const text = $('.reach-text', reach);
  if (!s) { reach.dataset.state = 'unknown'; text.textContent = 'Mengecek…'; return; }
  reach.dataset.state = s.up ? 'up' : 'down';
  text.textContent = s.up ? `${s.ms} ms` : 'Tidak terjangkau';
}

function renderSummary(list) {
  const up = list.filter((c) => state.status.get(c.id)?.up).length;
  $('#list-summary').textContent = list.length
    ? `${list.length} koneksi${state.status.size ? `, ${up} terjangkau` : ''}`
    : '';
}

function render() {
  renderGroups();
  const list = visible();
  const ul = $('#conns');
  const tpl = $('#row-tpl');

  $('#list-title').textContent = state.group === null ? 'Semua koneksi' : state.group;
  renderSummary(list);

  $('#empty').hidden = state.connections.length > 0;
  $('#no-match').hidden = !(state.connections.length > 0 && list.length === 0);
  ul.hidden = list.length === 0;

  ul.replaceChildren(...list.map((c) => {
    const li = tpl.content.firstElementChild.cloneNode(true);
    li.dataset.protocol = c.protocol;
    li.dataset.id = c.id;
    $('.proto', li).textContent = c.protocol.toUpperCase();
    $('.conn-name strong', li).textContent = c.name;
    $('.conn-name span', li).textContent = relTime(c.lastUsed);

    const addr = $('.conn-addr', li);
    if (c.username) addr.append(`${c.username}@`);
    const b = document.createElement('b');
    b.textContent = c.hostname;
    addr.append(b, `:${c.port}`);
    addr.title = `${c.username ? c.username + '@' : ''}${c.hostname}:${c.port}`;

    const open = $('a.btn', li);
    open.href = `/s/${c.id}`;
    open.setAttribute('aria-label', `Buka ${c.name}`);
    $('[data-action="edit"]', li).addEventListener('click', () => openDialog(c));
    li.addEventListener('dblclick', (e) => {
      if (e.target.closest('button, a')) return;
      window.open(`/s/${c.id}`, '_blank', 'noopener');
    });
    applyStatus(li, c.id);
    return li;
  }));
}

// ---------------------------------------------------------------- data
async function load() {
  state.connections = await api('/connections');
  render();
  refreshStatus();
}

let statusTimer;
async function refreshStatus() {
  clearTimeout(statusTimer);
  if (state.connections.length) {
    try {
      const results = await api('/status');
      state.status = new Map(results.map((r) => [r.id, r]));
      // Update dots in place so focus and scroll position stay put.
      document.querySelectorAll('#conns .conn').forEach((li) => applyStatus(li, Number(li.dataset.id)));
      renderSummary(visible());
    } catch { /* keep last known */ }
  }
  statusTimer = setTimeout(refreshStatus, 30000);
}

// -------------------------------------------------------------- dialog
const dlg = $('#dlg');
const form = $('#conn-form');

function setProtocol(p) {
  form.querySelectorAll('[data-for]').forEach((fs) => { fs.hidden = fs.dataset.for !== p; });
  const port = form.elements.port;
  if (!port.value || Object.values(DEFAULT_PORT).includes(Number(port.value))) port.value = DEFAULT_PORT[p];
  port.placeholder = DEFAULT_PORT[p];
}

function openDialog(conn = null) {
  state.editing = conn;
  form.reset();
  $('#form-error').textContent = '';
  $('#dlg-title').textContent = conn ? `Ubah ${conn.name}` : 'Tambah koneksi';
  $('#btn-delete').hidden = !conn;

  const p = conn?.protocol || 'rdp';
  form.elements.protocol.value = p;
  if (conn) {
    const f = form.elements;
    f.name.value = conn.name;
    f.group.value = conn.group;
    f.hostname.value = conn.hostname;
    f.port.value = conn.port;
    f.username.value = conn.username;
    f.notes.value = conn.notes;
    const o = conn.options || {};
    if (p === 'rdp') {
      f.domain.value = o.domain || '';
      f.security.value = o.security || 'any';
      f.ignoreCert.checked = o.ignoreCert !== false;
      f.audio.checked = o.audio !== false;
      f.wallpaper.checked = Boolean(o.wallpaper);
      f.quality.value = o.quality || 'fast';
    } else {
      f.fontSize.value = o.fontSize || 12;
      f.colorScheme.value = o.colorScheme || 'gray-black';
    }
  } else {
    form.elements.port.value = '';
  }
  $('#pw-hint').hidden = !conn?.hasPassword;
  $('#pw-clear-wrap').hidden = !conn?.hasPassword;
  $('#key-hint').hidden = !conn?.hasPrivateKey;
  $('#key-clear-wrap').hidden = !conn?.hasPrivateKey;
  setProtocol(p);
  dlg.showModal();
  form.elements.name.focus();
}

function collect() {
  const f = form.elements;
  const protocol = f.protocol.value;
  const body = {
    protocol,
    name: f.name.value,
    group: f.group.value,
    hostname: f.hostname.value,
    port: f.port.value === '' ? '' : Number(f.port.value),
    username: f.username.value,
    notes: f.notes.value,
    options: protocol === 'rdp'
      ? { domain: f.domain.value, security: f.security.value, quality: f.quality.value, ignoreCert: f.ignoreCert.checked, audio: f.audio.checked, wallpaper: f.wallpaper.checked }
      : { fontSize: Number(f.fontSize.value), colorScheme: f.colorScheme.value },
  };
  // Secrets: omit = keep, null = clear, value = replace.
  if (f.password.value) body.password = f.password.value;
  else if (f.clearPassword.checked || !state.editing) body.password = null;
  if (protocol === 'ssh') {
    if (f.privateKey.value.trim()) body.privateKey = f.privateKey.value.trim() + '\n';
    else if (f.clearKey.checked || !state.editing) body.privateKey = null;
    if (f.passphrase.value) body.passphrase = f.passphrase.value;
    else if (f.clearKey.checked) body.passphrase = null;
  }
  return body;
}

form.addEventListener('change', (e) => {
  if (e.target.name === 'protocol') setProtocol(e.target.value);
});

form.addEventListener('submit', async (e) => {
  e.preventDefault();
  const btn = $('#btn-save');
  btn.disabled = true;
  $('#form-error').textContent = '';
  try {
    const body = collect();
    if (state.editing) await api(`/connections/${state.editing.id}`, { method: 'PUT', body: JSON.stringify(body) });
    else await api('/connections', { method: 'POST', body: JSON.stringify(body) });
    dlg.close();
    toast(state.editing ? 'Perubahan disimpan' : 'Koneksi ditambahkan');
    await load();
  } catch (err) {
    $('#form-error').textContent = err.message;
  } finally {
    btn.disabled = false;
  }
});

$('#btn-delete').addEventListener('click', async () => {
  const c = state.editing;
  if (!c || !confirm(`Hapus koneksi “${c.name}”? Kredensial yang tersimpan ikut terhapus.`)) return;
  try {
    await api(`/connections/${c.id}`, { method: 'DELETE' });
    dlg.close();
    toast('Koneksi dihapus');
    await load();
  } catch (err) {
    $('#form-error').textContent = err.message;
  }
});

document.addEventListener('click', (e) => {
  const action = e.target.closest('[data-action]')?.dataset.action;
  if (action === 'close') dlg.close();
  if (action === 'add') openDialog();
});

$('#btn-add').addEventListener('click', () => openDialog());
$('#search').addEventListener('input', (e) => { state.query = e.target.value.trim(); render(); });
$('#btn-logout').addEventListener('click', async () => {
  await fetch('/api/logout', { method: 'POST' });
  location.replace('/login');
});

document.addEventListener('keydown', (e) => {
  if (e.key === '/' && !dlg.open && document.activeElement.tagName !== 'INPUT') {
    e.preventDefault();
    $('#search').focus();
  }
});

load().catch((err) => toast(err.message));
