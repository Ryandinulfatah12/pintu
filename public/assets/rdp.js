// RDP through guacd. The Pintu server relays the Guacamole protocol over /tunnel.
import Guacamole from '/vendor/guacamole-common.js';

const ERRORS = {
  0x0202: 'Server tujuan tidak merespons (timeout).',
  0x0203: 'Server tujuan menolak atau memutus koneksi.',
  0x0207: 'Host tidak ditemukan atau tidak bisa dijangkau dari server Pintu.',
  0x0208: 'Server tujuan sedang tidak tersedia.',
  0x0209: 'Sesi diambil alih dari tempat lain.',
  0x020a: 'Sesi berakhir karena tidak ada aktivitas.',
  0x020b: 'Sesi ditutup oleh server tujuan.',
  0x0301: 'Login ditolak. Periksa username dan password koneksi ini.',
  0x0308: 'Browser terlalu lama tidak merespons.',
};

let current = null; // the active session; keyboard and resize handling route to it

// ---------------------------------------------------------------- sizing
const dpr = () => Math.min(window.devicePixelRatio || 1, 2);
const wantedSize = (viewport) => ({
  width: Math.round(viewport.clientWidth * dpr()),
  height: Math.round(viewport.clientHeight * dpr()),
  dpi: Math.round(96 * dpr()),
});

function fit() {
  if (!current) return;
  const display = current.client.getDisplay();
  const dw = display.getWidth();
  const dh = display.getHeight();
  if (!dw || !dh) return;
  const vp = current.ui.viewport;
  display.scale(Math.min(vp.clientWidth / dw, vp.clientHeight / dh));
}

// WebP is the cheapest format for guacd to encode and send; offer it when the browser has it.
function imageTypes() {
  const types = ['image/jpeg', 'image/png'];
  try {
    const c = document.createElement('canvas');
    c.width = c.height = 1;
    if (c.toDataURL('image/webp').startsWith('data:image/webp')) types.push('image/webp');
  } catch { /* ignore */ }
  return types;
}

// Installed once per tab; Guacamole.Keyboard cannot be detached.
let globalsReady = false;
function installGlobals(ui) {
  if (globalsReady) return;
  globalsReady = true;

  let resizeTimer;
  new ResizeObserver(() => {
    fit();
    clearTimeout(resizeTimer);
    resizeTimer = setTimeout(() => {
      if (current?.connected) {
        const s = wantedSize(ui.viewport);
        current.client.sendSize(s.width, s.height);
      }
    }, 250);
  }).observe(ui.viewport);

  const keyboard = new Guacamole.Keyboard(document);
  keyboard.onkeydown = (keysym) => {
    if (!current?.connected || ui.typingLocally()) return true;
    current.client.sendKeyEvent(1, keysym);
    return false;
  };
  keyboard.onkeyup = (keysym) => {
    if (!current?.connected || ui.typingLocally()) return;
    current.client.sendKeyEvent(0, keysym);
  };
  window.addEventListener('blur', () => keyboard.reset());
}

export function start(info, ui) {
  installGlobals(ui);

  const tunnel = new Guacamole.WebSocketTunnel('/tunnel');
  const client = new Guacamole.Client(tunnel);
  const s = { client, ui, connected: false };
  current = s;

  const display = client.getDisplay();
  ui.display.replaceChildren(display.getElement());
  display.onresize = fit;

  // Render the remote cursor as the local one (no lag on the pointer).
  display.showCursor(false);
  display.oncursor = (canvas, x, y) => {
    ui.display.style.cursor = `url(${canvas.toDataURL()}) ${x} ${y}, auto`;
  };

  const onMouse = (e) => client.sendMouseState(e.state, true);
  const mouse = new Guacamole.Mouse(display.getElement());
  mouse.onEach(['mousedown', 'mouseup', 'mousemove'], onMouse);
  const touch = new Guacamole.Mouse.Touchscreen(display.getElement());
  touch.onEach(['mousedown', 'mouseup', 'mousemove'], onMouse);

  client.onclipboard = (stream, mimetype) => {
    if (!/^text\//.test(mimetype)) return;
    const reader = new Guacamole.StringReader(stream);
    let data = '';
    reader.ontext = (t) => { data += t; };
    reader.onend = () => ui.remoteClipboard(data);
  };

  client.onstatechange = (state) => {
    // 3 = CONNECTED, 5 = DISCONNECTED
    if (state === 3) {
      s.connected = true;
      ui.connected();
      fit();
      ui.display.focus();
    }
    if (state === 5) {
      s.connected = false;
      ui.closed();
    }
  };

  const fail = (status) => {
    s.connected = false;
    ui.failed(ERRORS[status.code] || status.message);
  };
  client.onerror = fail;
  tunnel.onerror = fail;

  const size = wantedSize(ui.viewport);
  const params = new URLSearchParams({ token: info.token, width: size.width, height: size.height, dpi: size.dpi });
  for (const t of imageTypes()) params.append('image', t);
  const tz = Intl.DateTimeFormat().resolvedOptions().timeZone;
  if (tz) params.set('timezone', tz);
  client.connect(params.toString());

  const sendClipboard = (text) => {
    if (!s.connected || !text) return;
    const stream = client.createClipboardStream('text/plain');
    const writer = new Guacamole.StringWriter(stream);
    writer.sendText(text);
    writer.sendEnd();
  };

  return {
    sendClipboard,
    syncClipboard: sendClipboard,
    sendCtrlAltDel() {
      if (!s.connected) return;
      const keys = [0xffe3, 0xffe9, 0xffff]; // Ctrl_L, Alt_L, Delete
      keys.forEach((k) => client.sendKeyEvent(1, k));
      keys.reverse().forEach((k) => client.sendKeyEvent(0, k));
      ui.display.focus();
    },
    focus: () => ui.display.focus(),
    disconnect: () => client.disconnect(),
    dispose() {
      client.onstatechange = client.onerror = tunnel.onerror = null;
      client.disconnect();
      if (current === s) current = null;
    },
  };
}
