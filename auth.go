package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	adminCookieName   = "vt_admin"
	adminSessionTTL   = 30 * 24 * time.Hour
	shareCookiePrefix = "vt_share_"
	shareCookieTTL    = 30 * 24 * time.Hour
	maxFailedAttempts = 10
	failedWindow      = 15 * time.Minute
)

// adminPassword is the shared admin password (ADMIN_PASSWORD). If empty,
// every admin route answers 503.
var adminPassword string

// sessionKey signs admin and share cookies (HMAC-SHA256).
var sessionKey []byte

// configureAuth reads ADMIN_PASSWORD / SESSION_SECRET and derives the
// cookie signing key.
func configureAuth() {
	adminPassword = os.Getenv("ADMIN_PASSWORD")
	secret := os.Getenv("SESSION_SECRET")
	switch {
	case secret != "":
		sessionKey = deriveKey(secret)
	case adminPassword != "":
		sessionKey = deriveKey(adminPassword)
	default:
		// No secret at all: random key, so share cookies still work
		// until the next restart.
		sessionKey = make([]byte, 32)
		if _, err := rand.Read(sessionKey); err != nil {
			log.Fatal(err)
		}
	}
	if adminPassword == "" {
		log.Println("ERROR: ADMIN_PASSWORD no configurada: el panel de administración responderá 503")
	}
}

func deriveKey(secret string) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("videotracker-session-v1"))
	return mac.Sum(nil)
}

func sign(payload string) string {
	mac := hmac.New(sha256.New, sessionKey)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// makeSignedValue returns "<expiryUnix>.<hmac(purpose|expiry)>".
func makeSignedValue(purpose string, expires time.Time) string {
	exp := strconv.FormatInt(expires.Unix(), 10)
	return exp + "." + sign(purpose+"|"+exp)
}

// checkSignedValue validates a value produced by makeSignedValue.
func checkSignedValue(purpose, value string) bool {
	exp, mac, ok := strings.Cut(value, ".")
	if !ok {
		return false
	}
	expUnix, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || time.Now().Unix() > expUnix {
		return false
	}
	return hmac.Equal([]byte(mac), []byte(sign(purpose+"|"+exp)))
}

func setCookie(w http.ResponseWriter, name, value, path string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     path,
		MaxAge:   int(ttl.Seconds()),
		Expires:  time.Now().Add(ttl),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

func isAdmin(r *http.Request) bool {
	if adminPassword == "" {
		return false
	}
	c, err := r.Cookie(adminCookieName)
	if err != nil {
		return false
	}
	return checkSignedValue("admin", c.Value)
}

func passwordMatches(given string) bool {
	// Compare fixed-size digests so length differences don't leak either.
	a := sha256.Sum256([]byte(given))
	b := sha256.Sum256([]byte(adminPassword))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// requireAdmin protects an admin page (redirect to login) or API (401 JSON).
func requireAdmin(handler http.HandlerFunc, api bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if adminPassword == "" {
			if api {
				writeJSONError(w, http.StatusServiceUnavailable, "ADMIN_PASSWORD no configurada")
			} else {
				http.Error(w, "ADMIN_PASSWORD no configurada", http.StatusServiceUnavailable)
			}
			return
		}
		if !isAdmin(r) {
			if api {
				writeJSONError(w, http.StatusUnauthorized, "no autorizado: inicia sesión en /admin/login")
				return
			}
			http.Redirect(w, r, "/admin/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		handler(w, r)
	}
}

// safeNext only allows local admin paths as post-login destination.
func safeNext(next string) string {
	if strings.HasPrefix(next, "/admin") && !strings.HasPrefix(next, "//") {
		return next
	}
	return "/admin"
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	if adminPassword == "" {
		http.Error(w, "ADMIN_PASSWORD no configurada", http.StatusServiceUnavailable)
		return
	}
	data := struct {
		Error string
		Next  string
	}{Next: safeNext(r.FormValue("next"))}

	switch r.Method {
	case http.MethodGet:
		if isAdmin(r) {
			http.Redirect(w, r, data.Next, http.StatusSeeOther)
			return
		}
	case http.MethodPost:
		ip := getClientIP(r)
		if loginLimiter.blocked(ip) {
			log.Printf("Login bloqueado por demasiados intentos - IP: %s", ip)
			w.WriteHeader(http.StatusTooManyRequests)
			data.Error = "Demasiados intentos fallidos. Prueba de nuevo dentro de 15 minutos."
			renderTemplate(w, "login.html", data)
			return
		}
		if passwordMatches(r.FormValue("password")) {
			loginLimiter.reset(ip)
			setCookie(w, adminCookieName, makeSignedValue("admin", time.Now().Add(adminSessionTTL)), "/", adminSessionTTL)
			log.Printf("Login de admin correcto - IP: %s", ip)
			http.Redirect(w, r, data.Next, http.StatusSeeOther)
			return
		}
		loginLimiter.fail(ip)
		log.Printf("Login de admin fallido - IP: %s", ip)
		w.WriteHeader(http.StatusUnauthorized)
		data.Error = "Contraseña incorrecta."
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	renderTemplate(w, "login.html", data)
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	setCookie(w, adminCookieName, "", "/", -time.Hour)
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

func renderTemplate(w http.ResponseWriter, name string, data any) {
	if err := templates.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("ERROR: Failed to render %s: %v", name, err)
	}
}

// ===== Rate limiting of failed password attempts =====

type attemptLimiter struct {
	mu       sync.Mutex
	failures map[string][]time.Time
}

func newAttemptLimiter() *attemptLimiter {
	return &attemptLimiter{failures: make(map[string][]time.Time)}
}

var (
	loginLimiter = newAttemptLimiter()
	shareLimiter = newAttemptLimiter()
)

// recent returns failures inside the window (caller holds the lock).
func (l *attemptLimiter) recent(key string) []time.Time {
	cutoff := time.Now().Add(-failedWindow)
	list := l.failures[key]
	i := 0
	for i < len(list) && list[i].Before(cutoff) {
		i++
	}
	list = list[i:]
	if len(list) == 0 {
		delete(l.failures, key)
	} else {
		l.failures[key] = list
	}
	return list
}

func (l *attemptLimiter) blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.recent(key)) >= maxFailedAttempts
}

func (l *attemptLimiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failures[key] = append(l.recent(key), time.Now())
	// Opportunistic cleanup so the map can't grow forever.
	if len(l.failures) > 10000 {
		for k := range l.failures {
			l.recent(k)
		}
	}
}

func (l *attemptLimiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, key)
}
