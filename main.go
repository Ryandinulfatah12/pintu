// Pintu: self-hosted remote desktop manager. SSH runs natively in Go
// (x/crypto/ssh + xterm.js), RDP goes through guacd.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed public
var embedded embed.FS

// ---------------------------------------------------------------- config

type config struct {
	port          int
	dataDir       string
	secretKey     string
	adminUser     string
	adminPassword string
	guacdAddr     string
	sessionHours  float64
	secureCookie  bool
	logLevel      int
}

const (
	logQuiet = iota
	logErrors
	logNormal
	logVerbose
	logDebug
)

var logLevel = logNormal

func logf(level int, format string, args ...any) {
	if level <= logLevel {
		log.Printf(format, args...)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func loadConfig() config {
	exe, _ := os.Executable()
	port, _ := strconv.Atoi(envOr("PORT", "8080"))
	hours, err := strconv.ParseFloat(envOr("SESSION_HOURS", "12"), 64)
	if err != nil || hours <= 0 {
		hours = 12
	}
	levels := map[string]int{"QUIET": logQuiet, "ERRORS": logErrors, "NORMAL": logNormal, "VERBOSE": logVerbose, "DEBUG": logDebug}
	level, ok := levels[strings.ToUpper(envOr("LOG_LEVEL", "NORMAL"))]
	if !ok {
		level = logNormal
	}
	return config{
		port:          port,
		dataDir:       envOr("DATA_DIR", filepath.Join(filepath.Dir(exe), "data")),
		secretKey:     os.Getenv("SECRET_KEY"),
		adminUser:     envOr("ADMIN_USER", "admin"),
		adminPassword: os.Getenv("ADMIN_PASSWORD"),
		guacdAddr:     net.JoinHostPort(envOr("GUACD_HOST", "127.0.0.1"), envOr("GUACD_PORT", "4822")),
		sessionHours:  hours,
		secureCookie:  os.Getenv("COOKIE_SECURE") == "true",
		logLevel:      level,
	}
}

// ------------------------------------------------------------------- app

type app struct {
	cfg    config
	store  *store
	auth   *auth
	tokens *tokenStore
	status *statusCache
}

// version is set at build time: -ldflags "-X main.version=v1.2.3".
var version = "dev"

func main() {
	args := os.Args[1:]
	configPath := ""
	if len(args) >= 2 && (args[0] == "--config" || args[0] == "-config") {
		configPath, args = args[1], args[2:]
	}

	if len(args) > 0 {
		switch args[0] {
		case "healthcheck":
			os.Exit(healthcheck())
		case "version", "--version":
			fmt.Println("pintu", version)
			return
		case "config":
			if len(args) != 3 || args[1] != "ensure-secret" {
				fatal(errors.New("pemakaian: pintu config ensure-secret <file>"))
			}
			fatal(ensureSecret(args[2]))
			return
		case "service":
			fatal(serviceCommand(args[1:], configPath))
			return
		default:
			fatal(fmt.Errorf("perintah tidak dikenal: %s", args[0]))
		}
	}

	if configPath == "" {
		exe, _ := os.Executable()
		if p := filepath.Join(filepath.Dir(exe), ".env"); fileExists(p) {
			configPath = p
		}
	}
	if configPath != "" {
		if err := loadEnvFile(configPath); err != nil {
			fatal(err)
		}
	}

	if isService() {
		fatal(runService())
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fatal(run(ctx))
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// openLogFile sends the log to path, rotating it to path.old once it grows past 10 MB.
func openLogFile(path string) error {
	if info, err := os.Stat(path); err == nil && info.Size() > 10<<20 {
		os.Rename(path, path+".old")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("gagal membuka LOG_FILE: %w", err)
	}
	log.SetOutput(f)
	return nil
}

// run serves Pintu until ctx is cancelled.
func run(ctx context.Context) error {
	if p := os.Getenv("LOG_FILE"); p != "" {
		if err := openLogFile(p); err != nil {
			return err
		}
	}

	cfg := loadConfig()
	logLevel = cfg.logLevel

	if len(cfg.secretKey) < 32 {
		return errors.New("SECRET_KEY wajib diisi, minimal 32 karakter. Contoh: openssl rand -hex 32")
	}
	if len(cfg.adminPassword) < 8 {
		return errors.New("ADMIN_PASSWORD wajib diisi, minimal 8 karakter.")
	}

	st, err := openStore(cfg.dataDir, cfg.secretKey)
	if err != nil {
		log.Printf("Gagal membuka database: %v", err)
		return fmt.Errorf("gagal membuka database: %w", err)
	}

	a := &app{
		cfg:    cfg,
		store:  st,
		auth:   newAuth(cfg),
		tokens: newTokenStore(),
		status: newStatusCache(),
	}

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.port),
		Handler:           a.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		log.Printf("Pintu %s jalan di http://0.0.0.0:%d (guacd: %s)", version, cfg.port, cfg.guacdAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Print(err)
			errc <- err
		}
	}()

	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)
	st.Close()
	return err
}

// healthcheck is used by the Docker HEALTHCHECK (the image has no shell or curl).
func healthcheck() int {
	client := http.Client{Timeout: 3 * time.Second}
	res, err := client.Get("http://127.0.0.1:" + envOr("PORT", "8080") + "/healthz")
	if err != nil {
		return 1
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

// ---------------------------------------------------------------- routes

func (a *app) routes() http.Handler {
	pub, _ := fs.Sub(embedded, "public")
	static := http.FileServerFS(pub)
	assets := func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") { // no directory listings
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=3600")
		static.ServeHTTP(w, r)
	}
	page := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { http.ServeFileFS(w, r, pub, name) }
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		if a.auth.readSession(r) != "" {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		page("login.html")(w, r)
	})
	mux.HandleFunc("GET /{$}", a.auth.requirePage(page("index.html")))
	mux.HandleFunc("GET /s/{id}", a.auth.requirePage(page("session.html")))
	mux.HandleFunc("GET /assets/", assets)
	mux.HandleFunc("GET /vendor/", assets)

	mux.HandleFunc("POST /api/login", a.auth.login)
	mux.HandleFunc("POST /api/logout", a.auth.logout)

	api := a.auth.requireAPI
	mux.HandleFunc("GET /api/me", api(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"user": a.auth.readSession(r)})
	}))
	mux.HandleFunc("GET /api/connections", api(a.listConnections))
	mux.HandleFunc("POST /api/connections", api(a.createConnection))
	mux.HandleFunc("GET /api/connections/{id}", api(a.getConnection))
	mux.HandleFunc("PUT /api/connections/{id}", api(a.updateConnection))
	mux.HandleFunc("DELETE /api/connections/{id}", api(a.deleteConnection))
	mux.HandleFunc("POST /api/connections/{id}/token", api(a.issueToken))
	mux.HandleFunc("GET /api/status", api(a.connectionStatus))
	mux.HandleFunc("/api/", api(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "Tidak ditemukan")
	}))

	mux.HandleFunc("GET /tunnel", a.handleGuacTunnel)
	mux.HandleFunc("GET /ssh", a.handleSSH)

	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// ------------------------------------------------------------------- API

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// readBody decodes a JSON object body (max 64 KB). An empty body yields an empty map.
func readBody(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "Body terlalu besar")
		return nil, false
	}
	body := map[string]any{}
	if len(strings.TrimSpace(string(data))) == 0 {
		return body, true
	}
	if err := json.Unmarshal(data, &body); err != nil || body == nil {
		writeError(w, http.StatusBadRequest, "Body JSON tidak valid")
		return nil, false
	}
	return body, true
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "ID tidak valid")
		return 0, false
	}
	return id, true
}

// storeError maps store errors to HTTP responses.
func storeError(w http.ResponseWriter, err error) {
	var ve validationError
	switch {
	case errors.As(err, &ve):
		writeError(w, http.StatusBadRequest, ve.Error())
	case errors.Is(err, errNotFound):
		writeError(w, http.StatusNotFound, "Koneksi tidak ditemukan")
	default:
		logf(logErrors, "store: %v", err)
		writeError(w, http.StatusInternalServerError, "Terjadi kesalahan di server")
	}
}

func (a *app) listConnections(w http.ResponseWriter, r *http.Request) {
	list, err := a.store.List()
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (a *app) createConnection(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	c, err := a.store.Create(body)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (a *app) getConnection(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	c, err := a.store.Get(id)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (a *app) updateConnection(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	c, err := a.store.Update(id, body)
	if err != nil {
		storeError(w, err)
		return
	}
	a.status.forget(id)
	writeJSON(w, http.StatusOK, c)
}

func (a *app) deleteConnection(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := a.store.Remove(id); err != nil {
		storeError(w, err)
		return
	}
	a.status.forget(id)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// issueToken hands the session tab a single-use token valid for 60 seconds.
// The connection and its credentials stay on the server; the token only points to them.
func (a *app) issueToken(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	t, err := a.store.GetForSession(id)
	if errors.Is(err, errNotFound) {
		storeError(w, err)
		return
	}
	if err != nil {
		logf(logErrors, "unseal connection %d: %v", id, err)
		writeError(w, http.StatusInternalServerError, "Kredensial tidak bisa dibuka. Apakah SECRET_KEY berubah?")
		return
	}
	a.store.Touch(id)

	resp := map[string]any{
		"token":    a.tokens.issue(t),
		"name":     t.Name,
		"protocol": t.Protocol,
		"hostname": t.Hostname,
	}
	if t.Protocol == "ssh" {
		resp["options"] = t.SSH
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *app) connectionStatus(w http.ResponseWriter, r *http.Request) {
	list, err := a.store.List()
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a.status.check(r.Context(), list))
}
