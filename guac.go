package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
)

// RDP sessions are relayed between guacamole-common-js in the browser and guacd.
// The relay only splits the guacd stream on instruction boundaries and answers
// the client's keep-alive pings; everything else passes through untouched.

const (
	guacStatusServerError = 0x0200
	guacStatusForbidden   = 0x0303
	clientIdleTimeout     = 60 * time.Second // client pings every 500 ms (slower in background tabs)
	guacdIdleTimeout      = 15 * time.Second // guacd sends sync/nop frames continuously
	maxPendingInstruction = 1 << 20
)

var errMalformed = errors.New("malformed guacamole instruction")

// ------------------------------------------------------------- protocol

// scanInstruction finds the end of the first instruction in b. It returns 0 if
// the instruction is incomplete. Element lengths count Unicode code points.
// If elems is non-nil, the decoded elements are appended to it.
func scanInstruction(b []byte, elems *[]string) (int, error) {
	i := 0
	for {
		n, start := 0, i
		for i < len(b) && b[i] >= '0' && b[i] <= '9' {
			n = n*10 + int(b[i]-'0')
			if n > maxPendingInstruction {
				return 0, errMalformed
			}
			i++
		}
		if i == len(b) {
			return 0, nil
		}
		if i == start || b[i] != '.' {
			return 0, errMalformed
		}
		i++
		valueStart := i
		for k := 0; k < n; k++ {
			if i >= len(b) {
				return 0, nil
			}
			if b[i] < utf8.RuneSelf {
				i++
				continue
			}
			if !utf8.FullRune(b[i:]) {
				return 0, nil
			}
			_, size := utf8.DecodeRune(b[i:])
			i += size
		}
		if i >= len(b) {
			return 0, nil
		}
		if elems != nil {
			*elems = append(*elems, string(b[valueStart:i]))
		}
		switch b[i] {
		case ',':
			i++
		case ';':
			return i + 1, nil
		default:
			return 0, errMalformed
		}
	}
}

func encodeInstruction(elems ...string) []byte {
	var buf bytes.Buffer
	for i, e := range elems {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(strconv.Itoa(utf8.RuneCountInString(e)))
		buf.WriteByte('.')
		buf.WriteString(e)
	}
	buf.WriteByte(';')
	return buf.Bytes()
}

// guacReader buffers a guacd stream and hands out whole instructions only,
// because the browser tunnel cannot parse an instruction split across messages.
type guacReader struct {
	conn net.Conn
	buf  []byte
	tmp  []byte
}

func newGuacReader(conn net.Conn) *guacReader {
	return &guacReader{conn: conn, tmp: make([]byte, 64<<10)}
}

func (g *guacReader) fill() error {
	if len(g.buf) > maxPendingInstruction {
		return errMalformed
	}
	g.conn.SetReadDeadline(time.Now().Add(guacdIdleTimeout))
	n, err := g.conn.Read(g.tmp)
	g.buf = append(g.buf, g.tmp[:n]...)
	if n > 0 {
		return nil
	}
	return err
}

// next returns one decoded instruction (used during the handshake).
func (g *guacReader) next() ([]string, error) {
	for {
		var elems []string
		end, err := scanInstruction(g.buf, &elems)
		if err != nil {
			return nil, err
		}
		if end > 0 {
			g.buf = g.buf[end:]
			return elems, nil
		}
		if err := g.fill(); err != nil {
			return nil, err
		}
	}
}

// chunk returns every complete instruction currently buffered, waiting for at least one.
func (g *guacReader) chunk() ([]byte, error) {
	for {
		total := 0
		for {
			end, err := scanInstruction(g.buf[total:], nil)
			if err != nil {
				return nil, err
			}
			if end == 0 {
				break
			}
			total += end
		}
		if total > 0 {
			out := slices.Clone(g.buf[:total])
			g.buf = append(g.buf[:0], g.buf[total:]...)
			return out, nil
		}
		if err := g.fill(); err != nil {
			return nil, err
		}
	}
}

// ------------------------------------------------------------- settings

// rdpSettings returns guacd connection parameters. "fast" trades colour depth
// and desktop effects for fewer, smaller screen updates.
func rdpSettings(t *target) map[string]string {
	o := t.RDP
	s := map[string]string{
		"hostname":                   t.Hostname,
		"port":                       strconv.Itoa(t.Port),
		"username":                   t.Username,
		"password":                   t.Secrets.Password,
		"domain":                     o.Domain,
		"security":                   o.Security,
		"ignore-cert":                strconv.FormatBool(o.IgnoreCert),
		"resize-method":              "display-update",
		"enable-wallpaper":           strconv.FormatBool(o.Wallpaper),
		"disable-audio":              strconv.FormatBool(!o.Audio),
		"enable-theming":             "false",
		"enable-menu-animations":     "false",
		"enable-full-window-drag":    "false",
		"enable-desktop-composition": "false",
	}
	if o.Quality == "quality" {
		s["color-depth"] = "24" // guacd falls back to 16-bit when unset
		s["enable-font-smoothing"] = "true"
		s["enable-theming"] = "true"
	} else {
		s["color-depth"] = "16"
		s["enable-font-smoothing"] = "false"
	}
	return s
}

var timezonePattern = regexp.MustCompile(`^[A-Za-z0-9_+\-/]{1,64}$`)

func queryInt(v string, def, lo, hi int) int {
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return max(lo, min(n, hi))
}

// ---------------------------------------------------------------- tunnel

func (a *app) handleGuacTunnel(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:    []string{"guacamole"},
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	defer ws.CloseNow()
	ws.SetReadLimit(maxPendingInstruction)

	ctx := r.Context()

	sendError := func(msg string, code int) {
		send(ctx, ws, websocket.MessageText, encodeInstruction("error", msg, strconv.Itoa(code)))
		ws.Close(websocket.StatusNormalClosure, "")
	}

	if a.auth.readSession(r) == "" {
		sendError("Sesi login berakhir. Silakan masuk lagi.", guacStatusForbidden)
		return
	}
	q := r.URL.Query()
	t := a.tokens.take(q.Get("token"))
	if t == nil || t.Protocol != "rdp" {
		sendError("Token sesi kedaluwarsa atau sudah dipakai. Hubungkan ulang.", guacStatusServerError)
		return
	}

	guacd, err := net.DialTimeout("tcp", a.cfg.guacdAddr, 5*time.Second)
	if err != nil {
		logf(logErrors, "guacd %s: %v", a.cfg.guacdAddr, err)
		sendError("guacd tidak bisa dihubungi. Pastikan container pintu-guacd berjalan.", guacStatusServerError)
		return
	}
	defer guacd.Close()

	reader := newGuacReader(guacd)
	id, err := a.guacHandshake(guacd, reader, t, q)
	if err != nil {
		var ge guacError
		if errors.As(err, &ge) {
			send(ctx, ws, websocket.MessageText, ge.raw)
			ws.Close(websocket.StatusNormalClosure, "")
			return
		}
		logf(logErrors, "guacd handshake (%s): %v", t.Name, err)
		sendError("Handshake dengan guacd gagal.", guacStatusServerError)
		return
	}
	logf(logNormal, "RDP %q (%s:%d) dibuka, guacd %s", t.Name, t.Hostname, t.Port, id)
	defer logf(logNormal, "RDP %q ditutup", t.Name)

	// The browser tunnel learns its UUID (and switches to OPEN) from this instruction.
	if err := send(ctx, ws, websocket.MessageText, encodeInstruction("", id)); err != nil {
		return
	}

	done := make(chan struct{}, 2)

	// guacd → browser
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			data, err := reader.chunk()
			if err != nil {
				if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
					logf(logVerbose, "guacd read (%s): %v", t.Name, err)
				}
				return
			}
			if err := send(ctx, ws, websocket.MessageText, data); err != nil {
				return
			}
		}
	}()

	// browser → guacd
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			readCtx, readCancel := context.WithTimeout(ctx, clientIdleTimeout)
			_, msg, err := ws.Read(readCtx)
			readCancel()
			if err != nil {
				return
			}
			// Internal instructions (empty opcode) are tunnel-level: answer pings, drop the rest.
			if bytes.HasPrefix(msg, []byte("0.,")) {
				if bytes.HasPrefix(msg, []byte("0.,4.ping,")) {
					send(ctx, ws, websocket.MessageText, msg)
				}
				continue
			}
			guacd.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := guacd.Write(msg); err != nil {
				return
			}
		}
	}()

	// Whichever side ends first: close the WebSocket cleanly, then guacd.
	<-done
	ws.Close(websocket.StatusNormalClosure, "")
	guacd.Close()
}

// guacError is an "error" instruction guacd sent during the handshake.
type guacError struct{ raw []byte }

func (e guacError) Error() string { return string(e.raw) }

// guacHandshake runs select → args → size/audio/video/image/timezone → connect → ready,
// and returns the guacd connection ID.
func (a *app) guacHandshake(conn net.Conn, reader *guacReader, t *target, q map[string][]string) (string, error) {
	get := func(k string) string {
		if v := q[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}

	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write(encodeInstruction("select", "rdp")); err != nil {
		return "", err
	}
	args, err := reader.next()
	if err != nil {
		return "", err
	}
	if len(args) == 0 || args[0] != "args" {
		if len(args) > 0 && args[0] == "error" {
			return "", guacError{encodeInstruction(args...)}
		}
		return "", fmt.Errorf("expected args, got %q", args)
	}
	args = args[1:]

	// Speak protocol 1.1.0 (adds timezone); newer guacd versions accept it.
	// A guacd that announces no version, or 1.0.0, gets 1.0.0.
	version := "VERSION_1_0_0"
	if len(args) > 0 && strings.HasPrefix(args[0], "VERSION_") && args[0] != "VERSION_1_0_0" {
		version = "VERSION_1_1_0"
	}

	settings := rdpSettings(t)
	connect := []string{"connect"}
	for _, name := range args {
		if strings.HasPrefix(name, "VERSION_") {
			connect = append(connect, version)
		} else {
			connect = append(connect, settings[name])
		}
	}

	width := queryInt(get("width"), 1024, 320, 8192)
	height := queryInt(get("height"), 768, 240, 8192)
	dpi := queryInt(get("dpi"), 96, 48, 384)

	images := []string{}
	for _, m := range q["image"] {
		if m == "image/png" || m == "image/jpeg" || m == "image/webp" {
			images = append(images, m)
		}
	}
	if len(images) == 0 {
		images = []string{"image/png", "image/jpeg"}
	}

	var hs bytes.Buffer
	hs.Write(encodeInstruction("size", strconv.Itoa(width), strconv.Itoa(height), strconv.Itoa(dpi)))
	hs.Write(encodeInstruction("audio", "audio/L16"))
	hs.Write(encodeInstruction("video"))
	hs.Write(encodeInstruction(append([]string{"image"}, images...)...))
	if version != "VERSION_1_0_0" {
		tz := get("timezone")
		if !timezonePattern.MatchString(tz) {
			tz = ""
		}
		hs.Write(encodeInstruction("timezone", tz))
	}
	hs.Write(encodeInstruction(connect...))
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write(hs.Bytes()); err != nil {
		return "", err
	}

	for {
		ins, err := reader.next()
		if err != nil {
			return "", err
		}
		switch {
		case len(ins) >= 2 && ins[0] == "ready":
			return ins[1], nil
		case len(ins) > 0 && ins[0] == "error":
			return "", guacError{encodeInstruction(ins...)}
		}
		logf(logDebug, "guacd handshake: ignoring %q", ins)
	}
}

// send writes one WebSocket message with a timeout so a stalled browser cannot block the relay.
func send(ctx context.Context, ws *websocket.Conn, typ websocket.MessageType, data []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return ws.Write(ctx, typ, data)
}
