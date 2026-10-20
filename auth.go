package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	sessionCookie = "pintu_session"
	maxFails      = 5
	lockDuration  = 5 * time.Minute
)

// auth handles the single admin login: an HMAC-signed session cookie
// ("<base64url user>.<expiry ms>.<signature>") plus a per-IP rate limit.
type auth struct {
	hmacKey  []byte
	user     string
	password string
	ttl      time.Duration
	secure   bool

	mu    sync.Mutex
	fails map[string]failEntry
}

type failEntry struct {
	count int
	until time.Time
}

func newAuth(cfg config) *auth {
	key := sha256.Sum256([]byte("pintu:session:" + cfg.secretKey))
	return &auth{
		hmacKey:  key[:],
		user:     cfg.adminUser,
		password: cfg.adminPassword,
		ttl:      time.Duration(cfg.sessionHours * float64(time.Hour)),
		secure:   cfg.secureCookie,
		fails:    map[string]failEntry{},
	}
}

func (a *auth) sign(data string) string {
	m := hmac.New(sha256.New, a.hmacKey)
	m.Write([]byte(data))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// safeEqual compares fixed-length digests so neither content nor length leaks through timing.
func safeEqual(a, b string) bool {
	da, db := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(da[:], db[:]) == 1
}

// readSession returns the logged-in user, or "" when there is no valid session.
func (a *auth) readSession(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return ""
	}
	parts := strings.Split(c.Value, ".")
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return ""
	}
	user64, exp, sig := parts[0], parts[1], parts[2]
	if !safeEqual(sig, a.sign(user64+"."+exp)) {
		return ""
	}
	expMs, err := strconv.ParseFloat(exp, 64)
	if err != nil || expMs < float64(time.Now().UnixMilli()) {
		return ""
	}
	user, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(user64, "="))
	if err != nil {
		return ""
	}
	return string(user)
}

func (a *auth) setCookie(w http.ResponseWriter, value string, maxAge int) {
	parts := []string{
		sessionCookie + "=" + value,
		"Path=/",
		"HttpOnly",
		"SameSite=Strict",
		fmt.Sprintf("Max-Age=%d", maxAge),
	}
	if a.secure {
		parts = append(parts, "Secure")
	}
	w.Header().Set("Set-Cookie", strings.Join(parts, "; "))
}

func clientIP(r *http.Request) string {
	if ip := r.Header.Get("Cf-Connecting-Ip"); ip != "" {
		return ip
	}
	if ip := r.Header.Get("X-Real-Ip"); ip != "" {
		return ip
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func (a *auth) login(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	now := time.Now()

	a.mu.Lock()
	entry, seen := a.fails[ip]
	a.mu.Unlock()
	if seen && entry.until.After(now) {
		mins := int(math.Ceil(entry.until.Sub(now).Minutes()))
		writeError(w, http.StatusTooManyRequests, fmt.Sprintf("Terlalu banyak percobaan. Coba lagi dalam %d menit.", mins))
		return
	}

	body, ok := readBody(w, r)
	if !ok {
		return
	}
	username, _ := body["username"].(string)
	password, _ := body["password"].(string)
	userOK := safeEqual(username, a.user)
	passOK := safeEqual(password, a.password) // both always evaluated
	if !userOK || !passOK {
		a.mu.Lock()
		count := 1
		if e, ok := a.fails[ip]; ok && (e.until.IsZero() || e.until.After(now)) {
			count = e.count + 1
		}
		var until time.Time
		if count >= maxFails {
			until = now.Add(lockDuration)
		}
		a.fails[ip] = failEntry{count: count, until: until}
		a.mu.Unlock()
		writeError(w, http.StatusUnauthorized, "Username atau password salah.")
		return
	}

	a.mu.Lock()
	delete(a.fails, ip)
	a.mu.Unlock()

	user64 := base64.RawURLEncoding.EncodeToString([]byte(a.user))
	exp := strconv.FormatInt(now.Add(a.ttl).UnixMilli(), 10)
	a.setCookie(w, user64+"."+exp+"."+a.sign(user64+"."+exp), int(a.ttl.Seconds()))
	writeJSON(w, http.StatusOK, map[string]string{"user": a.user})
}

func (a *auth) logout(w http.ResponseWriter, r *http.Request) {
	a.setCookie(w, "", 0)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *auth) requireAPI(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.readSession(r) == "" {
			writeError(w, http.StatusUnauthorized, "Sesi berakhir. Silakan masuk lagi.")
			return
		}
		next(w, r)
	}
}

func (a *auth) requirePage(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.readSession(r) == "" {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		next(w, r)
	}
}
