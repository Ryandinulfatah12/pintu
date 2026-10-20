package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"
)

// SSH sessions run natively: browser (xterm.js) ⇄ WebSocket ⇄ x/crypto/ssh ⇄ server.
// Binary frames carry raw terminal bytes in both directions; text frames carry
// small JSON control messages (resize from the browser, status from the server).

var errPromptCancelled = errors.New("login dibatalkan")

type hostKeyChangedError struct{ fingerprint string }

func (e hostKeyChangedError) Error() string {
	return "Host key server ini berubah (sekarang " + e.fingerprint + "). Bisa karena server diinstal ulang, " +
		"atau ada yang menyadap koneksi. Kalau kamu yakin aman, buka Ubah koneksi ini lalu Simpan untuk menerima key baru."
}

type friendlyError string

func (e friendlyError) Error() string { return string(e) }

// wsTerm is the browser terminal: an io.Writer for remote output plus an input queue.
type wsTerm struct {
	ctx     context.Context
	ws      *websocket.Conn
	input   chan []byte
	pending []byte // input left over after a login prompt

	mu         sync.Mutex
	cols, rows int
	resized    chan struct{}
	prompted   sync.Once
}

func (t *wsTerm) Write(p []byte) (int, error) {
	if err := send(t.ctx, t.ws, websocket.MessageBinary, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (t *wsTerm) writeString(s string) { t.Write([]byte(s)) }

func (t *wsTerm) control(v any) {
	b, _ := json.Marshal(v)
	send(t.ctx, t.ws, websocket.MessageText, b)
}

func (t *wsTerm) size() (int, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cols, t.rows
}

// readLoop feeds keyboard input into the queue and applies resize requests.
func (t *wsTerm) readLoop(cancel context.CancelFunc) {
	defer cancel()
	for {
		typ, msg, err := t.ws.Read(t.ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageBinary {
			select {
			case t.input <- msg:
			case <-t.ctx.Done():
				return
			}
			continue
		}
		var ctl struct {
			Type string `json:"type"`
			Cols int    `json:"cols"`
			Rows int    `json:"rows"`
		}
		if json.Unmarshal(msg, &ctl) == nil && ctl.Type == "resize" && ctl.Cols > 0 && ctl.Rows > 0 {
			t.mu.Lock()
			t.cols, t.rows = min(ctl.Cols, 1000), min(ctl.Rows, 500)
			t.mu.Unlock()
			select {
			case t.resized <- struct{}{}:
			default:
			}
		}
	}
}

func (t *wsTerm) nextInput() ([]byte, error) {
	if len(t.pending) > 0 {
		p := t.pending
		t.pending = nil
		return p, nil
	}
	select {
	case p := <-t.input:
		return p, nil
	case <-t.ctx.Done():
		return nil, t.ctx.Err()
	}
}

// prompt reads one line typed in the browser terminal, like OpenSSH's login prompts.
func (t *wsTerm) prompt(label string, echo bool) (string, error) {
	t.prompted.Do(func() { t.control(map[string]string{"type": "prompt"}) }) // reveal the terminal
	t.writeString(label)
	var line []rune
	esc := 0 // 1 = after ESC, 2 = inside an escape sequence
	for {
		chunk, err := t.nextInput()
		if err != nil {
			return "", err
		}
		s := string(chunk)
		for i, r := range s {
			switch esc {
			case 1:
				esc = 0
				if r == '[' || r == 'O' {
					esc = 2
				}
				continue
			case 2:
				if r >= 0x40 && r <= 0x7e {
					esc = 0
				}
				continue
			}
			switch {
			case r == 0x1b:
				esc = 1
			case r == '\r' || r == '\n':
				t.writeString("\r\n")
				if rest := s[i+1:]; rest != "" {
					t.pending = []byte(strings.TrimPrefix(rest, "\n"))
				}
				return string(line), nil
			case r == 0x03 || r == 0x04:
				t.writeString("^C\r\n")
				return "", errPromptCancelled
			case r == 0x7f || r == 0x08:
				if len(line) > 0 {
					line = line[:len(line)-1]
					if echo {
						t.writeString("\b \b")
					}
				}
			case r == 0x15: // Ctrl+U
				if echo {
					t.writeString(strings.Repeat("\b \b", len(line)))
				}
				line = line[:0]
			case r >= 0x20:
				line = append(line, r)
				if echo {
					t.writeString(string(r))
				}
			}
		}
	}
}

func (a *app) handleSSH(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer ws.CloseNow()
	ws.SetReadLimit(1 << 20)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	q := r.URL.Query()
	term := &wsTerm{
		ctx:     ctx,
		ws:      ws,
		input:   make(chan []byte, 256),
		cols:    queryInt(q.Get("cols"), 80, 10, 1000),
		rows:    queryInt(q.Get("rows"), 24, 5, 500),
		resized: make(chan struct{}, 1),
	}
	fail := func(msg string) {
		term.control(map[string]string{"type": "error", "message": msg})
		ws.Close(websocket.StatusNormalClosure, "")
	}

	if a.auth.readSession(r) == "" {
		fail("Sesi login berakhir. Silakan masuk lagi.")
		return
	}
	t := a.tokens.take(q.Get("token"))
	if t == nil || t.Protocol != "ssh" {
		fail("Token sesi kedaluwarsa atau sudah dipakai. Hubungkan ulang.")
		return
	}

	go term.readLoop(cancel)

	client, err := a.dialSSH(ctx, t, term)
	if err != nil {
		logf(logErrors, "SSH %q (%s:%d): %v", t.Name, t.Hostname, t.Port, err)
		fail(sshErrorMessage(err))
		return
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		fail("Server menolak membuka sesi: " + err.Error())
		return
	}
	defer session.Close()

	cols, rows := term.size()
	modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 115200, ssh.TTY_OP_OSPEED: 115200}
	if err := session.RequestPty("xterm-256color", rows, cols, modes); err != nil {
		fail("Server menolak membuat terminal (PTY): " + err.Error())
		return
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		fail("Gagal menyiapkan input terminal.")
		return
	}
	session.Stdout = term
	session.Stderr = term
	if err := session.Shell(); err != nil {
		fail("Server menolak membuka shell: " + err.Error())
		return
	}

	term.control(map[string]string{"type": "connected"})
	logf(logNormal, "SSH %q (%s:%d) dibuka", t.Name, t.Hostname, t.Port)
	defer logf(logNormal, "SSH %q ditutup", t.Name)

	// keyboard → server, and terminal resizes
	go func() {
		for {
			if len(term.pending) > 0 {
				stdin.Write(term.pending)
				term.pending = nil
			}
			select {
			case p := <-term.input:
				if _, err := stdin.Write(p); err != nil {
					cancel()
					return
				}
			case <-term.resized:
				c, r := term.size()
				session.WindowChange(r, c)
			case <-ctx.Done():
				return
			}
		}
	}()

	// Keep both legs alive and notice dead peers (also stops idle proxies from dropping the socket).
	go func() {
		tick := time.NewTicker(25 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				pingCtx, pingCancel := context.WithTimeout(ctx, 15*time.Second)
				err := ws.Ping(pingCtx)
				pingCancel()
				if err != nil {
					cancel()
					return
				}
				reply := make(chan error, 1)
				go func() { _, _, err := client.SendRequest("keepalive@openssh.com", true, nil); reply <- err }()
				select {
				case err := <-reply:
					if err != nil {
						cancel()
						return
					}
				case <-time.After(15 * time.Second):
					cancel()
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	exited := make(chan struct{})
	go func() { session.Wait(); close(exited) }()
	select {
	case <-exited:
	case <-ctx.Done():
	}
	ws.Close(websocket.StatusNormalClosure, "")
}

func (a *app) dialSSH(ctx context.Context, t *target, term *wsTerm) (*ssh.Client, error) {
	addr := net.JoinHostPort(t.Hostname, strconv.Itoa(t.Port))
	d := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}

	user := t.Username
	if user == "" {
		if user, err = term.prompt("login as: ", true); err != nil {
			conn.Close()
			return nil, err
		}
		if user == "" {
			conn.Close()
			return nil, errPromptCancelled
		}
	}

	var methods []ssh.AuthMethod
	if t.Secrets.PrivateKey != "" {
		signer, err := parsePrivateKey(t.Secrets.PrivateKey, t.Secrets.Passphrase)
		if err != nil {
			conn.Close()
			return nil, err
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}
	if pw := t.Secrets.Password; pw != "" {
		methods = append(methods, ssh.Password(pw), ssh.KeyboardInteractive(term.keyboardInteractive(pw)))
	} else {
		// No stored password: ask in the terminal, like OpenSSH does.
		ask := func() (string, error) { return term.prompt(user+"@"+t.Hostname+"'s password: ", false) }
		methods = append(methods,
			ssh.RetryableAuthMethod(ssh.PasswordCallback(ask), 3),
			ssh.RetryableAuthMethod(ssh.KeyboardInteractive(term.keyboardInteractive("")), 3))
	}

	// Trust on first use: pin the host key the first time, refuse a different one later.
	var firstKey ssh.PublicKey
	cfg := &ssh.ClientConfig{
		User: user,
		Auth: methods,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			if t.HostKey == "" {
				firstKey = key
				return nil
			}
			pinned, _, _, _, err := ssh.ParseAuthorizedKey([]byte(t.HostKey))
			if err == nil && bytes.Equal(pinned.Marshal(), key.Marshal()) {
				return nil
			}
			return hostKeyChangedError{ssh.FingerprintSHA256(key)}
		},
	}
	if t.HostKey != "" {
		if pinned, _, _, _, err := ssh.ParseAuthorizedKey([]byte(t.HostKey)); err == nil {
			cfg.HostKeyAlgorithms = hostKeyAlgorithms(pinned.Type())
		}
	}

	// Typing a password can take a while; bound the whole login, then clear the deadline.
	conn.SetDeadline(time.Now().Add(2 * time.Minute))
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	stop()
	if err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetDeadline(time.Time{})

	if firstKey != nil {
		key := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(firstKey)))
		if err := a.store.SetHostKey(t.ID, key); err != nil {
			logf(logErrors, "save host key %q: %v", t.Name, err)
		}
		term.writeString("\x1b[2mHost key disimpan: " + ssh.FingerprintSHA256(firstKey) + "\x1b[0m\r\n")
	}
	return ssh.NewClient(c, chans, reqs), nil
}

// keyboardInteractive answers hidden prompts with the stored password, or asks in the terminal.
func (t *wsTerm) keyboardInteractive(password string) ssh.KeyboardInteractiveChallenge {
	return func(name, instruction string, questions []string, echos []bool) ([]string, error) {
		if name != "" {
			t.writeString(name + "\r\n")
		}
		if instruction != "" {
			t.writeString(instruction + "\r\n")
		}
		answers := make([]string, len(questions))
		for i, q := range questions {
			if !echos[i] && password != "" {
				answers[i] = password
				continue
			}
			a, err := t.prompt(q, echos[i])
			if err != nil {
				return nil, err
			}
			answers[i] = a
		}
		return answers, nil
	}
}

func hostKeyAlgorithms(keyType string) []string {
	if keyType == ssh.KeyAlgoRSA {
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA}
	}
	return []string{keyType}
}

func parsePrivateKey(key, passphrase string) (ssh.Signer, error) {
	var signer ssh.Signer
	var err error
	if passphrase != "" {
		signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(key), []byte(passphrase))
	} else {
		signer, err = ssh.ParsePrivateKey([]byte(key))
	}
	var missing *ssh.PassphraseMissingError
	switch {
	case errors.As(err, &missing):
		return nil, friendlyError("Private key ini pakai passphrase. Isi passphrase-nya di pengaturan koneksi.")
	case err != nil && passphrase != "":
		return nil, friendlyError("Private key tidak bisa dibuka. Periksa passphrase-nya.")
	case err != nil:
		return nil, friendlyError("Private key tidak bisa dibaca: " + err.Error())
	}
	return signer, nil
}

func sshErrorMessage(err error) string {
	var hk hostKeyChangedError
	var fe friendlyError
	var ne net.Error
	var oe *net.OpError
	var dnsErr *net.DNSError
	msg := err.Error()
	switch {
	case errors.As(err, &hk):
		return hk.Error()
	case errors.As(err, &fe):
		return fe.Error()
	case errors.Is(err, errPromptCancelled):
		return "Login dibatalkan."
	case errors.As(err, &dnsErr):
		return "Host tidak ditemukan atau tidak bisa dijangkau dari server Pintu."
	case strings.Contains(msg, "connection refused"):
		return "Koneksi ditolak server tujuan. Pastikan SSH berjalan dan port-nya benar."
	case errors.As(err, &ne) && ne.Timeout():
		return "Server tujuan tidak merespons (timeout)."
	case strings.Contains(msg, "unable to authenticate"):
		return "Login ditolak. Periksa username, password, atau private key koneksi ini."
	case errors.As(err, &oe):
		return "Tidak bisa terhubung ke server tujuan: " + oe.Err.Error()
	}
	return fmt.Sprintf("SSH gagal: %v", err)
}
