package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"
)

const testKey = "0123456789abcdef0123456789abcdef0123456789abcdef"

func copyDB(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	data, err := os.ReadFile("testdata/node-v1.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pintu.db"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// testdata/node-v1.db and the cookie below were produced by the original Node.js version.
func TestNodeDatabaseCompat(t *testing.T) {
	st, err := openStore(copyDB(t), testKey)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	list, err := st.List()
	if err != nil || len(list) != 2 {
		t.Fatalf("List = %v, %v", list, err)
	}
	if list[0].Name != "Server ünïcode" || list[0].Port != 2222 || !list[0].HasPassword || !list[0].HasPrivateKey || list[0].LastUsed == nil {
		t.Errorf("ssh row = %+v", list[0])
	}

	ssh1, err := st.GetForSession(1)
	if err != nil {
		t.Fatal(err)
	}
	if ssh1.Secrets != (secrets{Password: "p@ss wörd", PrivateKey: "KEY\n"}) {
		t.Errorf("secrets = %+v", ssh1.Secrets)
	}
	if ssh1.SSH != (sshOptions{FontSize: 14, ColorScheme: "green-black"}) {
		t.Errorf("ssh options = %+v", ssh1.SSH)
	}

	rdp, err := st.GetForSession(2)
	if err != nil {
		t.Fatal(err)
	}
	want := rdpOptions{Domain: "CORP", Security: "nla", IgnoreCert: true, Wallpaper: true, Audio: true, Quality: "fast"}
	if rdp.RDP != want || rdp.Secrets.Password != "rdp-pass" || rdp.Group != "Rumah" {
		t.Errorf("rdp = %+v %+v", rdp.RDP, rdp.Secrets)
	}

	// Updating without secrets keeps them; the blob is re-sealed by Go.
	if _, err := st.Update(1, map[string]any{"protocol": "ssh", "name": "Baru", "hostname": "h", "port": ""}); err != nil {
		t.Fatal(err)
	}
	again, _ := st.GetForSession(1)
	if again.Secrets.Password != "p@ss wörd" || again.Port != 22 {
		t.Errorf("after update: %+v port %d", again.Secrets, again.Port)
	}

	dir := copyDB(t)
	wrong, err := openStore(dir, strings.Repeat("x", 32))
	if err != nil {
		t.Fatal(err)
	}
	defer wrong.Close()
	if _, err := wrong.GetForSession(1); err == nil {
		t.Error("expected unseal failure with a different SECRET_KEY")
	}
	if c, _ := wrong.Get(1); c.HasPassword {
		t.Error("wrong key should present secrets as empty")
	}

	// Editing under the wrong key must not wipe credentials it cannot read.
	if _, err := wrong.Update(1, map[string]any{"protocol": "ssh", "name": "Edit", "hostname": "h"}); err != nil {
		t.Fatal(err)
	}
	if _, err := wrong.Update(1, map[string]any{"protocol": "ssh", "name": "Edit", "hostname": "h", "password": "baru"}); err == nil {
		t.Error("partial secrets under a wrong key should be rejected")
	}
	right, err := openStore(dir, testKey)
	if err != nil {
		t.Fatal(err)
	}
	defer right.Close()
	kept, err := right.GetForSession(1)
	if err != nil || kept.Name != "Edit" || kept.Secrets != (secrets{Password: "p@ss wörd", PrivateKey: "KEY\n"}) {
		t.Errorf("after wrong-key edit: %+v, %v", kept, err)
	}
}

func TestNodeCookieCompat(t *testing.T) {
	a := newAuth(config{secretKey: testKey, adminUser: "ryan", sessionHours: 1})
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Cookie", "pintu_session=cnlhbg.2107826673052.szIaq72NxQTPiGML3pf8IdgImJLVZib7aRz8SBpxUOs")
	if got := a.readSession(r); got != "ryan" {
		t.Errorf("readSession = %q", got)
	}
	r.Header.Set("Cookie", "pintu_session=cnlhbg.2107826673053.szIaq72NxQTPiGML3pf8IdgImJLVZib7aRz8SBpxUOs")
	if got := a.readSession(r); got != "" {
		t.Errorf("tampered cookie accepted: %q", got)
	}
}

func TestValidation(t *testing.T) {
	st, _ := openStore(t.TempDir(), testKey)
	defer st.Close()
	cases := []struct {
		in   map[string]any
		want string
	}{
		{map[string]any{"protocol": "vnc", "name": "a", "hostname": "h"}, "Protokol harus RDP atau SSH"},
		{map[string]any{"protocol": "rdp", "hostname": "h"}, "Nama koneksi wajib diisi"},
		{map[string]any{"protocol": "rdp", "name": "a", "hostname": "a b"}, `Host tidak boleh berisi spasi atau "/"`},
		{map[string]any{"protocol": "rdp", "name": "a", "hostname": "h", "port": 70000.0}, "Port harus 1–65535"},
		{map[string]any{"protocol": "rdp", "name": 5.0, "hostname": "h"}, "Nama harus berupa teks"},
		{map[string]any{"protocol": "rdp", "name": strings.Repeat("é", 81), "hostname": "h"}, "Nama maksimal 80 karakter"},
	}
	for _, c := range cases {
		if _, err := st.Create(c.in); err == nil || err.Error() != c.want {
			t.Errorf("Create(%v) = %v, want %q", c.in, err, c.want)
		}
	}

	c, err := st.Create(map[string]any{"protocol": "ssh", "name": " a ", "hostname": "h", "port": "2200",
		"options": map[string]any{"fontSize": "40", "colorScheme": "nope"}, "password": nil})
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "a" || c.Port != 2200 || string(c.Options) != `{"fontSize":12,"colorScheme":"gray-black"}` {
		t.Errorf("created = %+v %s", c, c.Options)
	}
}

func TestRDPProfiles(t *testing.T) {
	fast := rdpSettings(&target{RDP: rdpOptions{Quality: "fast"}})
	if fast["color-depth"] != "16" || fast["enable-font-smoothing"] != "false" || fast["enable-theming"] != "false" {
		t.Errorf("fast = %v", fast)
	}
	q := rdpSettings(&target{RDP: rdpOptions{Quality: "quality"}})
	if q["color-depth"] != "24" || q["enable-font-smoothing"] != "true" || q["enable-theming"] != "true" {
		t.Errorf("quality = %v", q)
	}
}

func TestScanInstruction(t *testing.T) {
	full := encodeInstruction("name", "héllo, wörld;", "")
	if string(full) != "4.name,13.héllo, wörld;,0.;" {
		t.Fatalf("encode = %q", full)
	}
	for i := 0; i < len(full); i++ {
		if end, err := scanInstruction(full[:i], nil); end != 0 || err != nil {
			t.Fatalf("prefix %d: end=%d err=%v", i, end, err)
		}
	}
	var elems []string
	if end, err := scanInstruction(append(full, "3.nop;"...), &elems); end != len(full) || err != nil {
		t.Fatalf("end=%d err=%v", end, err)
	}
	if len(elems) != 3 || elems[1] != "héllo, wörld;" {
		t.Errorf("elems = %q", elems)
	}
	for _, bad := range []string{"x.nop;", "3.nop!", ".nop;"} {
		if _, err := scanInstruction([]byte(bad), nil); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}

// ------------------------------------------------------------ end to end

type testServer struct {
	*httptest.Server
	app    *app
	client *http.Client
}

func newTestServer(t *testing.T, guacdAddr string) *testServer {
	t.Helper()
	st, err := openStore(t.TempDir(), testKey)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config{secretKey: testKey, adminUser: "admin", adminPassword: "rahasia123", sessionHours: 1, guacdAddr: guacdAddr}
	a := &app{cfg: cfg, store: st, auth: newAuth(cfg), tokens: newTokenStore(), status: newStatusCache()}
	srv := httptest.NewServer(a.routes())
	t.Cleanup(func() { srv.Close(); st.Close() })
	jar, _ := cookiejar.New(nil)
	return &testServer{Server: srv, app: a, client: &http.Client{Jar: jar}}
}

func (s *testServer) call(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, s.URL+path, r)
	req.Header.Set("Content-Type", "application/json")
	res, err := s.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	data, _ := io.ReadAll(res.Body)
	json.Unmarshal(data, &out)
	return res.StatusCode, out
}

func (s *testServer) dial(t *testing.T, path string, subprotocols ...string) *websocket.Conn {
	t.Helper()
	u, _ := url.Parse(s.URL)
	h := http.Header{}
	for _, c := range s.client.Jar.Cookies(u) {
		h.Add("Cookie", c.Name+"="+c.Value)
	}
	ws, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(s.URL, "http")+path,
		&websocket.DialOptions{HTTPHeader: h, Subprotocols: subprotocols})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	return ws
}

func TestAPIFlow(t *testing.T) {
	s := newTestServer(t, "")

	if code, _ := s.call(t, "GET", "/api/connections", nil); code != 401 {
		t.Errorf("unauthenticated list = %d", code)
	}
	if code, _ := s.call(t, "POST", "/api/login", map[string]string{"username": "admin", "password": "salah"}); code != 401 {
		t.Errorf("bad login = %d", code)
	}
	if code, _ := s.call(t, "POST", "/api/login", map[string]string{"username": "admin", "password": "rahasia123"}); code != 200 {
		t.Fatalf("login = %d", code)
	}
	code, c := s.call(t, "POST", "/api/connections", map[string]any{"protocol": "rdp", "name": "PC", "hostname": "127.0.0.1", "password": "x"})
	if code != 201 || c["hasPassword"] != true || c["lastUsed"] != nil {
		t.Fatalf("create = %d %v", code, c)
	}
	if _, ok := c["password"]; ok {
		t.Error("password leaked to the browser")
	}
	code, tok := s.call(t, "POST", "/api/connections/1/token", nil)
	if code != 200 || len(tok["token"].(string)) != 32 || tok["protocol"] != "rdp" {
		t.Errorf("token = %d %v", code, tok)
	}
	if code, _ := s.call(t, "GET", "/api/connections/abc", nil); code != 400 {
		t.Errorf("bad id = %d", code)
	}
	if code, _ := s.call(t, "DELETE", "/api/connections/9", nil); code != 404 {
		t.Errorf("delete missing = %d", code)
	}
	if code, _ := s.call(t, "GET", "/api/nope", nil); code != 404 {
		t.Errorf("unknown api = %d", code)
	}

	res, _ := s.client.Get(s.URL + "/s/1")
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !bytes.Contains(body, []byte("session.js")) {
		t.Errorf("session page = %d", res.StatusCode)
	}
	res, _ = s.client.Get(s.URL + "/vendor/guacamole-common.js")
	res.Body.Close()
	if res.StatusCode != 200 || res.Header.Get("Cache-Control") == "" {
		t.Errorf("vendor = %d", res.StatusCode)
	}
}

// fakeGuacd accepts one connection, checks the handshake and then streams instructions.
func fakeGuacd(t *testing.T, got chan<- []string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := newGuacReader(conn)
		sel, _ := r.next()
		got <- sel
		conn.Write(encodeInstruction("args", "VERSION_1_5_0", "hostname", "port", "password", "color-depth", "domain"))
		for {
			ins, err := r.next()
			if err != nil {
				return
			}
			got <- ins
			if ins[0] == "connect" {
				break
			}
		}
		conn.Write(encodeInstruction("ready", "$abc"))
		// One instruction split mid-character, across two writes.
		msg := encodeInstruction("name", "Désktop")
		conn.Write(msg[:8])
		time.Sleep(50 * time.Millisecond)
		conn.Write(append(msg[8:], encodeInstruction("sync", "1")...))
		ins, err := r.next() // something the browser sent
		if err == nil {
			got <- ins
		}
	}()
	return ln.Addr().String()
}

func TestGuacTunnel(t *testing.T) {
	got := make(chan []string, 16)
	s := newTestServer(t, fakeGuacd(t, got))
	s.call(t, "POST", "/api/login", map[string]string{"username": "admin", "password": "rahasia123"})
	s.call(t, "POST", "/api/connections", map[string]any{"protocol": "rdp", "name": "PC", "hostname": "10.1.1.1", "password": "pw",
		"options": map[string]any{"domain": "CORP"}})
	_, tok := s.call(t, "POST", "/api/connections/1/token", nil)

	ws := s.dial(t, "/tunnel?token="+tok["token"].(string)+"&width=1280&height=720&dpi=96&image=image/webp&image=evil&timezone=Asia/Jakarta", "guacamole")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	read := func() string {
		_, msg, err := ws.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return string(msg)
	}
	if m := read(); m != "0.,4.$abc;" {
		t.Errorf("uuid instruction = %q", m)
	}
	var stream string
	for !strings.Contains(stream, "4.sync") {
		stream += read()
	}
	if stream != "4.name,7.Désktop;4.sync,1.1;" {
		t.Errorf("stream = %q", stream)
	}

	ws.Write(ctx, websocket.MessageText, []byte("0.,4.ping,13.1700000000000;"))
	if m := read(); m != "0.,4.ping,13.1700000000000;" {
		t.Errorf("ping echo = %q", m)
	}
	ws.Write(ctx, websocket.MessageText, encodeInstruction("key", "65", "1"))

	want := map[string][]string{
		"select":   {"select", "rdp"},
		"size":     {"size", "1280", "720", "96"},
		"image":    {"image", "image/webp"},
		"timezone": {"timezone", "Asia/Jakarta"},
		"connect":  {"connect", "VERSION_1_1_0", "10.1.1.1", "3389", "pw", "16", "CORP"},
		"key":      {"key", "65", "1"},
	}
	deadline := time.After(3 * time.Second)
	for len(want) > 0 {
		select {
		case ins := <-got:
			if w, ok := want[ins[0]]; ok {
				if fmt.Sprint(ins) != fmt.Sprint(w) {
					t.Errorf("%s = %q, want %q", ins[0], ins, w)
				}
				delete(want, ins[0])
			}
		case <-deadline:
			t.Fatalf("guacd never saw %v", want)
		}
	}

	// The token is single use.
	ws2 := s.dial(t, "/tunnel?token="+tok["token"].(string), "guacamole")
	if _, msg, _ := ws2.Read(ctx); !strings.HasPrefix(string(msg), "5.error,") {
		t.Errorf("reused token = %q", msg)
	}
}

// fakeSSHServer runs an SSH server whose shell echoes input in upper case.
func fakeSSHServer(t *testing.T, password string) (addr string, hostKey ssh.PublicKey) {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	cfg := &ssh.ServerConfig{PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
		if c.User() == "ryan" && string(pw) == password {
			return nil, nil
		}
		return nil, fmt.Errorf("denied")
	}}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, chans, reqs, err := ssh.NewServerConn(nc, cfg)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				for nch := range chans {
					ch, chReqs, _ := nch.Accept()
					go func() {
						for req := range chReqs {
							req.Reply(req.Type == "pty-req" || req.Type == "shell" || req.Type == "window-change", nil)
						}
					}()
					go func() {
						ch.Write([]byte("welcome\r\n"))
						buf := make([]byte, 256)
						for {
							n, err := ch.Read(buf)
							if err != nil {
								return
							}
							if bytes.Contains(buf[:n], []byte("exit")) {
								ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
								ch.Close()
								return
							}
							ch.Write(bytes.ToUpper(buf[:n]))
						}
					}()
				}
			}()
		}
	}()
	return ln.Addr().String(), signer.PublicKey()
}

func TestSSHSession(t *testing.T) {
	addr, hostKey := fakeSSHServer(t, "s3cret")
	host, port, _ := net.SplitHostPort(addr)
	p, _ := strconv.Atoi(port)

	s := newTestServer(t, "")
	s.call(t, "POST", "/api/login", map[string]string{"username": "admin", "password": "rahasia123"})
	s.call(t, "POST", "/api/connections", map[string]any{"protocol": "ssh", "name": "srv", "hostname": host, "port": p, "username": "ryan"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	readUntil := func(ws *websocket.Conn, want string) string {
		var all string
		for !strings.Contains(all, want) {
			_, msg, err := ws.Read(ctx)
			if err != nil {
				t.Fatalf("waiting for %q, got %q: %v", want, all, err)
			}
			all += string(msg)
		}
		return all
	}

	// No stored password: the server prompts in the terminal.
	_, tok := s.call(t, "POST", "/api/connections/1/token", nil)
	ws := s.dial(t, "/ssh?cols=100&rows=30&token="+tok["token"].(string))
	readUntil(ws, `"type":"prompt"`)
	readUntil(ws, "ryan@"+host+"'s password: ")
	ws.Write(ctx, websocket.MessageBinary, []byte("s3cret\r"))
	readUntil(ws, "Host key disimpan: "+ssh.FingerprintSHA256(hostKey))
	readUntil(ws, `"type":"connected"`)
	readUntil(ws, "welcome")
	ws.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":120,"rows":40}`))
	ws.Write(ctx, websocket.MessageBinary, []byte("halo"))
	readUntil(ws, "HALO")
	ws.Write(ctx, websocket.MessageBinary, []byte("exit"))
	if _, _, err := ws.Read(ctx); websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Errorf("after exit: %v", err)
	}

	pinned, _ := s.app.store.GetForSession(1)
	if !strings.HasPrefix(pinned.HostKey, "ssh-ed25519 ") {
		t.Fatalf("host key not pinned: %q", pinned.HostKey)
	}

	// A different host key on the same address is refused.
	other, _ := fakeSSHServer(t, "s3cret")
	otherHost, otherPort, _ := net.SplitHostPort(other)
	s.app.store.db.Exec(`UPDATE connections SET hostname=?, port=? WHERE id=1`, otherHost, otherPort)
	_, tok = s.call(t, "POST", "/api/connections/1/token", nil)
	ws = s.dial(t, "/ssh?token="+tok["token"].(string))
	if got := readUntil(ws, `"type":"error"`); !strings.Contains(got, "Host key server ini berubah") {
		t.Errorf("changed host key: %q", got)
	}
}
