// Session tab shell: overlay, toolbar and clipboard panel. The protocol itself
// lives in rdp.js (guacd via guacamole-common-js) or ssh.js (native, xterm.js),
// loaded on demand so each tab only downloads what it needs.

const $ = (id) => document.getElementById(id);
const id = Number(location.pathname.split('/').pop());

const clipBox = $('clip');
const clipText = $('clip-text');

let session = null; // controller returned by rdp.js / ssh.js
let userEnded = false;

// --------------------------------------------------------------- overlay
function showOverlay(title, text = '', { busy = false, actions = false } = {}) {
  $('overlay').hidden = false;
  $('ov-title').textContent = title;
  $('ov-text').textContent = text;
  $('ov-spin').hidden = !busy;
  $('ov-actions').hidden = !actions;
}
const hideOverlay = () => { $('overlay').hidden = true; };

// Hooks the protocol modules call back into.
const ui = {
  viewport: $('viewport'),
  display: $('display'),
  connected() {
    hideOverlay();
  },
  failed(message) {
    showOverlay('Koneksi gagal', message || 'Koneksi terputus tanpa keterangan.', { actions: true });
  },
  closed(title = 'Koneksi terputus', text = 'Sesi berhenti tanpa pesan error.') {
    if (userEnded) showOverlay('Sesi diputus', 'Kamu bisa menutup tab ini atau menghubungkan ulang.', { actions: true });
    else if ($('overlay').hidden) showOverlay(title, text, { actions: true });
  },
  remoteClipboard(text) {
    lastRemoteClip = text;
    clipText.value = text;
    navigator.clipboard?.writeText(text).catch(() => {});
  },
  typingLocally() {
    const el = document.activeElement;
    return Boolean(el && clipBox.contains(el));
  },
};

// ------------------------------------------------------------- clipboard
let lastRemoteClip = '';

function toggleClip(open = clipBox.hidden) {
  clipBox.hidden = !open;
  $('btn-clip').setAttribute('aria-expanded', String(open));
  $('hud').classList.toggle('pinned', open);
  if (open) clipText.focus();
  else session?.focus();
}

$('btn-clip').addEventListener('click', () => toggleClip());
$('clip-close').addEventListener('click', () => toggleClip(false));
$('clip-send').addEventListener('click', () => {
  if (clipText.value) session?.sendClipboard(clipText.value);
  toggleClip(false);
});

// RDP only: push the local clipboard to the remote whenever this tab regains focus.
// (For SSH that would type the clipboard into the shell, so ssh.js has no syncClipboard.)
window.addEventListener('focus', async () => {
  if (!session?.syncClipboard || !navigator.clipboard?.readText) return;
  try {
    const text = await navigator.clipboard.readText();
    if (text && text !== lastRemoteClip) session.syncClipboard(text);
  } catch { /* permission denied or insecure context */ }
});

// --------------------------------------------------------------- connect
async function connect() {
  userEnded = false;
  session?.dispose();
  session = null;
  showOverlay('Menghubungkan…', '', { busy: true });

  let info;
  try {
    const res = await fetch(`/api/connections/${id}/token`, { method: 'POST' });
    if (res.status === 401) return location.replace('/login');
    info = await res.json();
    if (!res.ok) throw new Error(info.error || 'Gagal menyiapkan sesi');
  } catch (err) {
    return showOverlay('Tidak bisa memulai sesi', err.message, { actions: true });
  }

  document.title = `${info.name} — Pintu`;
  $('hud-title').replaceChildren(info.name, Object.assign(document.createElement('small'), { textContent: info.protocol.toUpperCase() }));
  $('btn-cad').hidden = info.protocol !== 'rdp';
  $('clip-help').textContent = info.protocol === 'ssh'
    ? 'Teks yang kamu blok di terminal muncul di sini. Tempel teks lalu kirim untuk mengetikkannya ke terminal.'
    : 'Teks yang disalin di remote muncul di sini. Tempel teks lalu kirim untuk memasukkannya ke clipboard remote.';
  showOverlay(`Menghubungkan ke ${info.name}…`, info.hostname, { busy: true });

  try {
    const mod = info.protocol === 'ssh' ? await import('./ssh.js?v=2') : await import('./rdp.js?v=2');
    session = await mod.start(info, ui);
  } catch (err) {
    showOverlay('Tidak bisa memulai sesi', err.message, { actions: true });
  }
}

// --------------------------------------------------------------- toolbar
$('btn-cad').addEventListener('click', () => session?.sendCtrlAltDel?.());

$('btn-full').addEventListener('click', () => {
  if (document.fullscreenElement) document.exitFullscreen();
  else document.documentElement.requestFullscreen?.().catch(() => {});
});
document.addEventListener('fullscreenchange', () => {
  $('btn-full').textContent = document.fullscreenElement ? 'Keluar layar penuh' : 'Layar penuh';
});

$('btn-end').addEventListener('click', () => {
  userEnded = true;
  session?.disconnect();
});

$('btn-retry').addEventListener('click', connect);

window.addEventListener('beforeunload', () => session?.disconnect());

if (Number.isInteger(id) && id > 0) connect();
else showOverlay('Koneksi tidak ditemukan', 'Alamat sesi ini tidak valid.', { actions: true });
