// Native SSH: xterm.js ⇄ /ssh WebSocket ⇄ Go (x/crypto/ssh). Binary frames are raw
// terminal bytes; text frames are JSON control messages.

const THEMES = {
  'gray-black': { background: '#000000', foreground: '#c8c8c8', cursor: '#c8c8c8' },
  'green-black': { background: '#000000', foreground: '#4ae36b', cursor: '#4ae36b' },
  'white-black': { background: '#000000', foreground: '#ffffff', cursor: '#ffffff' },
  'black-white': { background: '#ffffff', foreground: '#000000', cursor: '#000000', selectionBackground: '#b5c7f0' },
};

const loadScript = (src) => new Promise((resolve, reject) => {
  const el = Object.assign(document.createElement('script'), { src, onload: resolve });
  el.onerror = () => reject(new Error(`Gagal memuat ${src}`));
  document.head.append(el);
});

let libs;
function loadXterm() {
  libs ??= (async () => {
    document.head.append(Object.assign(document.createElement('link'), { rel: 'stylesheet', href: '/vendor/xterm/xterm.css' }));
    await loadScript('/vendor/xterm/xterm.js');
    await Promise.all([loadScript('/vendor/xterm/addon-fit.js'), loadScript('/vendor/xterm/addon-webgl.js')]);
    await document.fonts?.load('14px "IBM Plex Mono"').catch(() => {});
  })();
  return libs;
}

const encoder = new TextEncoder();

export async function start(info, ui) {
  await loadXterm();

  const opts = info.options || {};
  const theme = THEMES[opts.colorScheme] || THEMES['gray-black'];
  document.body.classList.add('ssh');
  ui.display.style.background = theme.background;

  const host = document.createElement('div');
  host.className = 'term';
  ui.display.replaceChildren(host);

  const term = new window.Terminal({
    fontFamily: '"IBM Plex Mono", ui-monospace, Menlo, Consolas, monospace',
    fontSize: opts.fontSize || 12,
    theme,
    cursorBlink: true,
    scrollback: 5000,
  });
  const fit = new window.FitAddon.FitAddon();
  term.loadAddon(fit);
  term.open(host);
  // GPU rendering keeps keystroke-to-pixel time low; fall back to the DOM renderer if WebGL is unavailable.
  try {
    const webgl = new window.WebglAddon.WebglAddon();
    webgl.onContextLoss(() => webgl.dispose());
    term.loadAddon(webgl);
  } catch { /* DOM renderer */ }
  fit.fit();

  // Ctrl+Shift+C copies, Ctrl+Shift+V pastes (plain Ctrl+C/V go to the shell).
  term.attachCustomKeyEventHandler((e) => {
    if (e.type !== 'keydown' || !e.ctrlKey || !e.shiftKey) return true;
    if (e.code === 'KeyC') {
      e.preventDefault(); // otherwise Chromium opens DevTools (inspect element)
      const sel = term.getSelection();
      if (sel) ui.remoteClipboard(sel);
      return false;
    }
    if (e.code === 'KeyV') return false; // let the browser fire a paste event
    return true;
  });
  term.onSelectionChange(() => {
    const sel = term.getSelection();
    if (sel) ui.remoteClipboard(sel);
  });

  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const params = new URLSearchParams({ token: info.token, cols: term.cols, rows: term.rows });
  const ws = new WebSocket(`${proto}//${location.host}/ssh?${params}`);
  ws.binaryType = 'arraybuffer';

  let connected = false;
  let failed = false;

  const sendBytes = (bytes) => { if (ws.readyState === WebSocket.OPEN) ws.send(bytes); };
  term.onData((d) => sendBytes(encoder.encode(d)));
  term.onBinary((d) => sendBytes(Uint8Array.from(d, (c) => c.charCodeAt(0))));
  term.onResize(({ cols, rows }) => {
    if (ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: 'resize', cols, rows }));
  });

  ws.onmessage = (e) => {
    if (typeof e.data !== 'string') {
      term.write(new Uint8Array(e.data));
      return;
    }
    const msg = JSON.parse(e.data);
    if (msg.type === 'prompt') {
      ui.connected(); // the server asks for a username/password in the terminal
      term.focus();
    } else if (msg.type === 'connected') {
      connected = true;
      ui.connected();
      fit.fit();
      term.focus();
    } else if (msg.type === 'error') {
      failed = true;
      ui.failed(msg.message);
    }
  };
  ws.onclose = () => {
    if (failed) return;
    if (connected) ui.closed('Sesi SSH selesai', 'Shell di server sudah ditutup.');
    else ui.failed('Koneksi ke server Pintu terputus.');
  };

  let raf = 0;
  const observer = new ResizeObserver(() => {
    cancelAnimationFrame(raf);
    raf = requestAnimationFrame(() => fit.fit());
  });
  observer.observe(ui.viewport);

  return {
    sendClipboard: (text) => term.paste(text),
    focus: () => term.focus(),
    disconnect: () => ws.close(),
    dispose() {
      ws.onclose = ws.onmessage = null;
      ws.close();
      observer.disconnect();
      term.dispose();
      document.body.classList.remove('ssh');
    },
  };
}
