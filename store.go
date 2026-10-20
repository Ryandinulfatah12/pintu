package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"

	_ "modernc.org/sqlite"
)

var defaultPorts = map[string]int{"rdp": 3389, "ssh": 22}

var (
	rdpSecurity = []string{"any", "nla", "tls", "rdp"}
	rdpQuality  = []string{"fast", "quality"}
	sshSchemes  = []string{"gray-black", "black-white", "white-black", "green-black"}
)

var errNotFound = errors.New("not found")

type validationError string

func (e validationError) Error() string { return string(e) }

// Connection is what the browser sees. Secrets never leave the server.
type Connection struct {
	ID            int64           `json:"id"`
	Name          string          `json:"name"`
	Group         string          `json:"group"`
	Protocol      string          `json:"protocol"`
	Hostname      string          `json:"hostname"`
	Port          int             `json:"port"`
	Username      string          `json:"username"`
	Options       json.RawMessage `json:"options"`
	Notes         string          `json:"notes"`
	LastUsed      *string         `json:"lastUsed"`
	HasPassword   bool            `json:"hasPassword"`
	HasPrivateKey bool            `json:"hasPrivateKey"`
}

type rdpOptions struct {
	Domain     string `json:"domain"`
	Security   string `json:"security"`
	IgnoreCert bool   `json:"ignoreCert"`
	Wallpaper  bool   `json:"wallpaper"`
	Audio      bool   `json:"audio"`
	Quality    string `json:"quality"` // fast = low latency, quality = full colour + effects
}

type sshOptions struct {
	FontSize    int    `json:"fontSize"`
	ColorScheme string `json:"colorScheme"`
}

type secrets struct {
	Password   string `json:"password,omitempty"`
	PrivateKey string `json:"privateKey,omitempty"`
	Passphrase string `json:"passphrase,omitempty"`
}

// target is a connection with decrypted credentials, used only server-side to open a session.
type target struct {
	Connection
	Secrets secrets
	RDP     rdpOptions
	SSH     sshOptions
	HostKey string // SSH host key pinned on first connect (authorized_keys format)
}

type store struct {
	db  *sql.DB
	gcm cipher.AEAD
}

const schema = `
CREATE TABLE IF NOT EXISTS connections (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  name       TEXT    NOT NULL,
  grp        TEXT    NOT NULL DEFAULT '',
  protocol   TEXT    NOT NULL CHECK (protocol IN ('rdp', 'ssh')),
  hostname   TEXT    NOT NULL,
  port       INTEGER NOT NULL,
  username   TEXT    NOT NULL DEFAULT '',
  secrets    TEXT    NOT NULL DEFAULT '',
  options    TEXT    NOT NULL DEFAULT '{}',
  notes      TEXT    NOT NULL DEFAULT '',
  last_used  TEXT,
  created_at TEXT    NOT NULL DEFAULT (datetime('now')),
  updated_at TEXT    NOT NULL DEFAULT (datetime('now'))
);`

func openStore(dataDir, secretKey string) (*store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	dsn := "file:" + filepath.ToSlash(filepath.Join(dataDir, "pintu.db")) +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	if err := addColumn(db, "host_key", "TEXT NOT NULL DEFAULT ''"); err != nil {
		db.Close()
		return nil, err
	}

	// Password / private key / passphrase are sealed with AES-256-GCM.
	key := sha256.Sum256([]byte("pintu:secrets:" + secretKey))
	block, _ := aes.NewCipher(key[:])
	gcm, _ := cipher.NewGCM(block)
	return &store{db: db, gcm: gcm}, nil
}

func addColumn(db *sql.DB, name, def string) error {
	rows, err := db.Query(`SELECT name FROM pragma_table_info('connections')`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			return err
		}
		if col == name {
			return nil
		}
	}
	_, err = db.Exec(fmt.Sprintf("ALTER TABLE connections ADD COLUMN %s %s", name, def))
	return err
}

func (s *store) Close() error { return s.db.Close() }

// ------------------------------------------------------------ encryption

// seal stores iv(12) | tag(16) | ciphertext, base64 — the same layout as the Node version.
func (s *store) seal(sec secrets) string {
	if sec == (secrets{}) {
		return ""
	}
	plain, _ := json.Marshal(sec)
	iv := make([]byte, 12)
	rand.Read(iv)
	out := s.gcm.Seal(nil, iv, plain, nil) // ciphertext | tag
	ct, tag := out[:len(out)-16], out[len(out)-16:]
	return base64.StdEncoding.EncodeToString(slices.Concat(iv, tag, ct))
}

func (s *store) unseal(blob string) (secrets, error) {
	var sec secrets
	if blob == "" {
		return sec, nil
	}
	buf, err := base64.StdEncoding.DecodeString(blob)
	if err != nil || len(buf) < 28 {
		return sec, errors.New("secrets blob is malformed")
	}
	plain, err := s.gcm.Open(nil, buf[:12], slices.Concat(buf[28:], buf[12:28]), nil)
	if err != nil {
		return sec, err
	}
	err = json.Unmarshal(plain, &sec)
	return sec, err
}

// ------------------------------------------------------------------ rows

type row struct {
	id                                               int64
	name, grp, protocol, hostname, username, secrets string
	options, notes, hostKey                          string
	port                                             int
	lastUsed                                         sql.NullString
}

const selectCols = `id, name, grp, protocol, hostname, port, username, secrets, options, notes, last_used, host_key`

func scanRow(sc interface{ Scan(...any) error }) (row, error) {
	var r row
	err := sc.Scan(&r.id, &r.name, &r.grp, &r.protocol, &r.hostname, &r.port, &r.username,
		&r.secrets, &r.options, &r.notes, &r.lastUsed, &r.hostKey)
	return r, err
}

func (s *store) public(r row) Connection {
	sec, _ := s.unseal(r.secrets) // wrong SECRET_KEY → treat as empty
	opts := json.RawMessage(r.options)
	if !json.Valid(opts) {
		opts = json.RawMessage("{}")
	}
	c := Connection{
		ID:            r.id,
		Name:          r.name,
		Group:         r.grp,
		Protocol:      r.protocol,
		Hostname:      r.hostname,
		Port:          r.port,
		Username:      r.username,
		Options:       opts,
		Notes:         r.notes,
		HasPassword:   sec.Password != "",
		HasPrivateKey: sec.PrivateKey != "",
	}
	if r.lastUsed.Valid {
		c.LastUsed = &r.lastUsed.String
	}
	return c
}

func (s *store) getRow(id int64) (row, error) {
	r, err := scanRow(s.db.QueryRow(`SELECT `+selectCols+` FROM connections WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return r, errNotFound
	}
	return r, err
}

func (s *store) List() ([]Connection, error) {
	rows, err := s.db.Query(`SELECT ` + selectCols + ` FROM connections ORDER BY grp COLLATE NOCASE, name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Connection{}
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, s.public(r))
	}
	return list, rows.Err()
}

func (s *store) Get(id int64) (Connection, error) {
	r, err := s.getRow(id)
	if err != nil {
		return Connection{}, err
	}
	return s.public(r), nil
}

// GetForSession returns the connection with decrypted secrets. Fails if SECRET_KEY changed.
func (s *store) GetForSession(id int64) (*target, error) {
	r, err := s.getRow(id)
	if err != nil {
		return nil, err
	}
	sec, err := s.unseal(r.secrets)
	if err != nil {
		return nil, err
	}
	t := &target{
		Connection: s.public(r),
		Secrets:    sec,
		RDP:        rdpOptions{Security: "any", IgnoreCert: true, Audio: true, Quality: "fast"},
		SSH:        sshOptions{FontSize: 12, ColorScheme: "gray-black"},
		HostKey:    r.hostKey,
	}
	if r.protocol == "rdp" {
		json.Unmarshal([]byte(r.options), &t.RDP)
	} else {
		json.Unmarshal([]byte(r.options), &t.SSH)
	}
	return t, nil
}

func (s *store) Create(input map[string]any) (Connection, error) {
	v, err := s.validate(input, nil)
	if err != nil {
		return Connection{}, err
	}
	res, err := s.db.Exec(`INSERT INTO connections (name, grp, protocol, hostname, port, username, secrets, options, notes)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		v.name, v.grp, v.protocol, v.hostname, v.port, v.username, v.secrets, v.options, v.notes)
	if err != nil {
		return Connection{}, err
	}
	id, _ := res.LastInsertId()
	return s.Get(id)
}

// Update replaces the connection. Saving also clears the pinned SSH host key,
// which is how a user accepts a new key after reinstalling a server.
func (s *store) Update(id int64, input map[string]any) (Connection, error) {
	existing, err := s.getRow(id)
	if err != nil {
		return Connection{}, err
	}
	v, err := s.validate(input, &existing)
	if err != nil {
		return Connection{}, err
	}
	_, err = s.db.Exec(`UPDATE connections SET name=?, grp=?, protocol=?, hostname=?, port=?, username=?,
		secrets=?, options=?, notes=?, host_key='', updated_at=datetime('now') WHERE id=?`,
		v.name, v.grp, v.protocol, v.hostname, v.port, v.username, v.secrets, v.options, v.notes, id)
	if err != nil {
		return Connection{}, err
	}
	return s.Get(id)
}

func (s *store) Touch(id int64) {
	s.db.Exec(`UPDATE connections SET last_used = datetime('now') WHERE id = ?`, id)
}

func (s *store) SetHostKey(id int64, key string) error {
	_, err := s.db.Exec(`UPDATE connections SET host_key = ? WHERE id = ?`, key, id)
	return err
}

func (s *store) Remove(id int64) error {
	res, err := s.db.Exec(`DELETE FROM connections WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNotFound
	}
	return nil
}

// ------------------------------------------------------------ validation

type validated struct {
	name, grp, protocol, hostname, username, secrets, options, notes string
	port                                                             int
}

// jsLen counts UTF-16 code units, matching the browser's maxlength.
func jsLen(s string) int { return len(utf16.Encode([]rune(s))) }

func str(v any, max int, field string) (string, error) {
	if v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", validationError(field + " harus berupa teks")
	}
	s = strings.TrimSpace(s)
	if jsLen(s) > max {
		return "", validationError(fmt.Sprintf("%s maksimal %d karakter", field, max))
	}
	return s, nil
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0 && !math.IsNaN(x)
	case string:
		return x != ""
	default:
		return true
	}
}

func number(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	}
	return 0, false
}

func oneOf(v any, allowed []string, def string) string {
	if s, ok := v.(string); ok && slices.Contains(allowed, s) {
		return s
	}
	return def
}

func cleanOptions(protocol string, raw any) (string, error) {
	o, _ := raw.(map[string]any)
	var opts any
	if protocol == "rdp" {
		domain, err := str(o["domain"], 120, "Domain")
		if err != nil {
			return "", err
		}
		opts = rdpOptions{
			Domain:     domain,
			Security:   oneOf(o["security"], rdpSecurity, "any"),
			IgnoreCert: o["ignoreCert"] != false,
			Wallpaper:  truthy(o["wallpaper"]),
			Audio:      o["audio"] != false,
			Quality:    oneOf(o["quality"], rdpQuality, "fast"),
		}
	} else {
		size := 12
		if f, ok := number(o["fontSize"]); ok && f == math.Trunc(f) && f >= 8 && f <= 32 {
			size = int(f)
		}
		opts = sshOptions{FontSize: size, ColorScheme: oneOf(o["colorScheme"], sshSchemes, "gray-black")}
	}
	b, _ := json.Marshal(opts)
	return string(b), nil
}

func (s *store) validate(input map[string]any, existing *row) (validated, error) {
	var v validated

	protocol, _ := input["protocol"].(string)
	if input["protocol"] == nil && existing != nil {
		protocol = existing.protocol
	}
	if _, ok := defaultPorts[protocol]; !ok {
		return v, validationError("Protokol harus RDP atau SSH")
	}
	v.protocol = protocol

	var err error
	if v.name, err = str(input["name"], 80, "Nama"); err != nil {
		return v, err
	}
	if v.hostname, err = str(input["hostname"], 253, "Host"); err != nil {
		return v, err
	}
	if v.name == "" {
		return v, validationError("Nama koneksi wajib diisi")
	}
	if v.hostname == "" {
		return v, validationError("Host wajib diisi")
	}
	if strings.ContainsAny(v.hostname, " \t\r\n\v\f/") {
		return v, validationError(`Host tidak boleh berisi spasi atau "/"`)
	}

	if p := input["port"]; p == nil || p == "" {
		v.port = defaultPorts[protocol]
	} else if f, ok := number(p); ok && f == math.Trunc(f) && f >= 1 && f <= 65535 {
		v.port = int(f)
	} else {
		return v, validationError("Port harus 1–65535")
	}

	// Secrets: missing = keep existing, null/"" = clear, string = replace.
	var prev secrets
	var prevErr error
	if existing != nil {
		prev, prevErr = s.unseal(existing.secrets)
	}
	pick := func(key, field, old string) (string, error) {
		val, present := input[key]
		if !present {
			if prevErr != nil {
				return "", validationError("Kredensial tersimpan tidak bisa dibuka dengan SECRET_KEY sekarang. Isi ulang semua kredensial koneksi ini (atau centang hapus), atau kembalikan SECRET_KEY lama.")
			}
			return old, nil
		}
		if !truthy(val) {
			return "", nil
		}
		text, ok := val.(string)
		if !ok {
			return "", validationError(field + " harus berupa teks")
		}
		return text, nil
	}
	_, hasPassword := input["password"]
	_, hasKey := input["privateKey"]
	_, hasPassphrase := input["passphrase"]
	if prevErr != nil && !hasPassword && !hasKey && !hasPassphrase {
		// Undecryptable (e.g. SECRET_KEY changed): leave the blob as is so that
		// restoring the old key brings the credentials back.
		v.secrets = existing.secrets
	} else {
		var sec secrets
		if sec.Password, err = pick("password", "Password", prev.Password); err != nil {
			return v, err
		}
		if protocol == "ssh" {
			if sec.PrivateKey, err = pick("privateKey", "Private key", prev.PrivateKey); err != nil {
				return v, err
			}
			if sec.Passphrase, err = pick("passphrase", "Passphrase", prev.Passphrase); err != nil {
				return v, err
			}
		}
		if jsLen(sec.Password) > 1024 {
			return v, validationError("Password terlalu panjang")
		}
		if jsLen(sec.PrivateKey) > 16384 {
			return v, validationError("Private key terlalu panjang")
		}
		v.secrets = s.seal(sec)
	}

	if v.grp, err = str(input["group"], 60, "Grup"); err != nil {
		return v, err
	}
	if v.username, err = str(input["username"], 120, "Username"); err != nil {
		return v, err
	}
	if v.options, err = cleanOptions(protocol, input["options"]); err != nil {
		return v, err
	}
	if v.notes, err = str(input["notes"], 2000, "Catatan"); err != nil {
		return v, err
	}
	return v, nil
}
