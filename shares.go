package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // Europe/Madrid in the alpine image

	"github.com/mattn/go-sqlite3"
	"golang.org/x/crypto/bcrypt"
)

// Share is a personalised public link /v/{token} to a video.
type Share struct {
	ID             int64      `json:"id"`
	Token          string     `json:"token"`
	VideoID        string     `json:"video_id"`
	VideoTitle     string     `json:"video_title"`
	Recipient      string     `json:"recipient"`
	CompanyName    string     `json:"company_name"`
	CompanyURL     string     `json:"company_url"`
	CompanyLogoURL string     `json:"company_logo_url"`
	BrandColor     string     `json:"brand_color"`
	PasswordHash   string     `json:"-"`
	HasPassword    bool       `json:"has_password"`
	ExpiresAt      *time.Time `json:"expires_at"`
	CreatedAt      time.Time  `json:"created_at"`
	Revoked        bool       `json:"revoked"`

	// Computed for the admin list.
	Status         string     `json:"status"`
	URL            string     `json:"url,omitempty"`
	OGImageURL     string     `json:"og_image_url,omitempty"`
	Visits         int        `json:"visits"`
	WatchedSeconds int        `json:"watched_seconds"`
	LastVisit      *time.Time `json:"last_visit"`
}

// OGMeta holds the Open Graph tags printed on public pages.
type OGMeta struct {
	Title       string
	Description string
	Image       string
	URL         string
}

const (
	genericOGTitle       = "Tu demo de SolNow"
	genericOGDescription = "Mira cómo gestionar reservas, pagos y clientes en un solo sitio con SolNow."
	ogDescriptionSuffix  = "· Mira cómo gestionar reservas, pagos y clientes en un solo sitio."
)

var (
	ogDir      = "og"
	tokenRegex = regexp.MustCompile(`^[A-Za-z0-9_-]{10,64}$`)
	madrid     *time.Location
)

func init() {
	var err error
	if madrid, err = time.LoadLocation("Europe/Madrid"); err != nil {
		madrid = time.UTC
	}
}

func createShareTables() {
	_, err := db.Exec(`
	CREATE TABLE IF NOT EXISTS shares (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		token TEXT UNIQUE NOT NULL,
		video_id TEXT NOT NULL,
		recipient TEXT NOT NULL,
		company_name TEXT DEFAULT '',
		company_url TEXT DEFAULT '',
		company_logo_url TEXT DEFAULT '',
		brand_color TEXT DEFAULT '',
		password_hash TEXT DEFAULT '',
		expires_at DATETIME,
		created_at DATETIME NOT NULL,
		revoked INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS idx_shares_video_id ON shares(video_id);
	`)
	if err != nil {
		log.Fatal(err)
	}
	ensureColumn("sessions", "share_token", "TEXT")
	if _, err := db.Exec("CREATE INDEX IF NOT EXISTS idx_sessions_share_token ON sessions(share_token)"); err != nil {
		log.Fatal(err)
	}
}

// ensureColumn adds a column if it does not exist yet (idempotent migration).
func ensureColumn(table, column, decl string) {
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?", table, column).Scan(&count); err != nil {
		log.Fatal(err)
	}
	if count > 0 {
		return
	}
	if _, err := db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, decl)); err != nil {
		log.Fatal(err)
	}
	log.Printf("Added %s column to %s table", column, table)
}

func newToken() string {
	b := make([]byte, 12) // 16 URL-safe chars
	if _, err := rand.Read(b); err != nil {
		log.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *Share) status(now time.Time) string {
	switch {
	case s.Revoked:
		return "revocado"
	case s.ExpiresAt != nil && now.After(*s.ExpiresAt):
		return "caducado"
	default:
		return "activo"
	}
}

func (s *Share) ogTitle() string {
	if s.CompanyName == "" {
		return genericOGTitle
	}
	return s.CompanyName + ", tu demo de SolNow"
}

func (s *Share) ogDescription() string {
	if s.VideoTitle == "" {
		return genericOGDescription
	}
	return s.VideoTitle + " " + ogDescriptionSuffix
}

const shareColumns = `s.id, s.token, s.video_id, COALESCE(v.title, ''), s.recipient, COALESCE(s.company_name, ''),
	COALESCE(s.company_url, ''), COALESCE(s.company_logo_url, ''), COALESCE(s.brand_color, ''),
	COALESCE(s.password_hash, ''), s.expires_at, s.created_at, s.revoked`

type rowScanner interface{ Scan(dest ...any) error }

func scanShare(row rowScanner, extra ...any) (*Share, error) {
	var s Share
	var expires sql.NullTime
	dest := append([]any{&s.ID, &s.Token, &s.VideoID, &s.VideoTitle, &s.Recipient, &s.CompanyName,
		&s.CompanyURL, &s.CompanyLogoURL, &s.BrandColor, &s.PasswordHash, &expires, &s.CreatedAt, &s.Revoked}, extra...)
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	if expires.Valid {
		s.ExpiresAt = &expires.Time
	}
	s.HasPassword = s.PasswordHash != ""
	s.Status = s.status(time.Now())
	return &s, nil
}

func getShareByToken(token string) (*Share, error) {
	return scanShare(db.QueryRow(`SELECT `+shareColumns+` FROM shares s
		LEFT JOIN videos v ON v.video_id = s.video_id WHERE s.token = ?`, token))
}

func getShareByID(id int64) (*Share, error) {
	return scanShare(db.QueryRow(`SELECT `+shareColumns+` FROM shares s
		LEFT JOIN videos v ON v.video_id = s.video_id WHERE s.id = ?`, id))
}

// baseURL returns the public scheme://host. PUBLIC_BASE_URL wins; behind
// the HTTPS Gateway we trust X-Forwarded-Proto.
func baseURL(r *http.Request) string {
	if b := strings.TrimRight(os.Getenv("PUBLIC_BASE_URL"), "/"); b != "" {
		return b
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// isPreviewBot detects link-preview crawlers (WhatsApp, Facebook, ...), so
// they don't create tracking sessions.
func isPreviewBot(ua string) bool {
	ua = strings.ToLower(ua)
	for _, b := range []string{"whatsapp", "facebookexternalhit", "facebot", "twitterbot", "telegrambot",
		"slackbot", "linkedinbot", "discordbot", "skypeuripreview", "googlebot", "bingbot", "applebot",
		"preview", "crawler", "spider", "bot/"} {
		if strings.Contains(ua, b) {
			return true
		}
	}
	return false
}

// ===== Public: /v/{token} =====

func shareCookieName(token string) string { return shareCookiePrefix + token }

func sharePurpose(s *Share) string {
	h := s.PasswordHash
	if len(h) > 16 {
		h = h[len(h)-16:]
	}
	return "share|" + s.Token + "|" + h
}

func hasShareAccess(r *http.Request, s *Share) bool {
	if !s.HasPassword {
		return true
	}
	c, err := r.Cookie(shareCookieName(s.Token))
	return err == nil && checkSignedValue(sharePurpose(s), c.Value)
}

func genericOG(r *http.Request) *OGMeta {
	b := baseURL(r)
	return &OGMeta{
		Title:       genericOGTitle,
		Description: genericOGDescription,
		Image:       b + "/og/solnow.jpg",
		URL:         b + r.URL.Path,
	}
}

func renderShareGone(w http.ResponseWriter, r *http.Request, status int, title, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	renderTemplate(w, "share_expired.html", struct {
		OG      *OGMeta
		Title   string
		Message string
	}{genericOG(r), title, message})
}

func handleShare(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.URL.Path, "/v/")
	if !tokenRegex.MatchString(token) {
		renderShareGone(w, r, http.StatusNotFound, "Enlace no encontrado", "Revisa que el enlace esté completo o pide uno nuevo a quien te lo envió.")
		return
	}
	s, err := getShareByToken(token)
	if errors.Is(err, sql.ErrNoRows) {
		renderShareGone(w, r, http.StatusNotFound, "Enlace no encontrado", "Revisa que el enlace esté completo o pide uno nuevo a quien te lo envió.")
		return
	} else if err != nil {
		log.Printf("ERROR: Failed to load share: %v", err)
		http.Error(w, "Error interno", http.StatusInternalServerError)
		return
	}
	if s.Status != "activo" || s.VideoTitle == "" {
		renderShareGone(w, r, http.StatusGone, "Este enlace ha caducado", "Si quieres volver a ver la demo, pide un enlace nuevo al equipo de SolNow.")
		return
	}

	b := baseURL(r)
	og := &OGMeta{
		Title:       s.ogTitle(),
		Description: s.ogDescription(),
		Image:       b + "/og/" + s.Token + ".jpg" + ogVersion(s.Token),
		URL:         b + "/v/" + s.Token,
	}
	w.Header().Set("Cache-Control", "no-store")

	if !hasShareAccess(r, s) {
		pageData := struct {
			OG          *OGMeta
			Error       string
			CompanyName string
		}{OG: og, CompanyName: s.CompanyName}
		if r.Method == http.MethodPost {
			ip := getClientIP(r)
			if shareLimiter.blocked(ip) {
				w.WriteHeader(http.StatusTooManyRequests)
				pageData.Error = "Demasiados intentos fallidos. Prueba de nuevo dentro de 15 minutos."
				renderTemplate(w, "share_password.html", pageData)
				return
			}
			if bcrypt.CompareHashAndPassword([]byte(s.PasswordHash), []byte(r.FormValue("password"))) == nil {
				shareLimiter.reset(ip)
				setCookie(w, shareCookieName(s.Token), makeSignedValue(sharePurpose(s), time.Now().Add(shareCookieTTL)), "/v/"+s.Token, shareCookieTTL)
				http.Redirect(w, r, "/v/"+s.Token, http.StatusSeeOther)
				return
			}
			shareLimiter.fail(ip)
			log.Printf("Contraseña de enlace incorrecta - token: %s, IP: %s", s.Token, ip)
			w.WriteHeader(http.StatusUnauthorized)
			pageData.Error = "Contraseña incorrecta."
		}
		renderTemplate(w, "share_password.html", pageData)
		return
	}
	if r.Method == http.MethodPost { // already unlocked
		http.Redirect(w, r, "/v/"+s.Token, http.StatusSeeOther)
		return
	}

	var videoURL string
	if err := db.QueryRow("SELECT url FROM videos WHERE video_id = ?", s.VideoID).Scan(&videoURL); err != nil {
		renderShareGone(w, r, http.StatusGone, "Este enlace ha caducado", "El vídeo ya no está disponible.")
		return
	}

	var sessionID int64
	if !isPreviewBot(r.UserAgent()) {
		res, err := db.Exec(
			"INSERT INTO sessions (user_id, video_id, ip_address, entered_at, share_token) VALUES (?, ?, ?, ?, ?)",
			s.Recipient, s.VideoID, getClientIP(r), time.Now(), s.Token,
		)
		if err != nil {
			log.Printf("ERROR: Failed to create share session: %v", err)
			http.Error(w, "Error interno", http.StatusInternalServerError)
			return
		}
		sessionID, _ = res.LastInsertId()
		log.Printf("Share session created - token: %s, recipient: %s, session: %d", s.Token, s.Recipient, sessionID)
	}

	renderTemplate(w, "watch_standalone.html", watchData{
		UserID:     s.Recipient,
		VideoID:    s.VideoID,
		VideoURL:   videoURL,
		VideoTitle: s.VideoTitle,
		SessionID:  sessionID,
		PageURL:    og.URL,
		VideoThumb: og.Image,
		OG:         og,
	})
}

// ===== Public: /og/{token}.jpg =====

func ogPath(token string) string { return filepath.Join(ogDir, token+".jpg") }

// ogVersion busts WhatsApp/CDN caches when the image is regenerated.
func ogVersion(token string) string {
	if fi, err := os.Stat(ogPath(token)); err == nil {
		return "?v=" + strconv.FormatInt(fi.ModTime().Unix(), 36)
	}
	return ""
}

var (
	genericOGOnce sync.Once
	genericOGJPEG []byte
)

func genericOGImage() []byte {
	genericOGOnce.Do(func() {
		var err error
		if genericOGJPEG, err = encodeOGJPEG(renderOGImage(OGParams{})); err != nil {
			log.Printf("ERROR: generic OG image: %v", err)
		}
	})
	return genericOGJPEG
}

func handleOGImage(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/og/")
	token, ok := strings.CutSuffix(name, ".jpg")
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	if !ok || !tokenRegex.MatchString(token) {
		if name == "solnow.jpg" {
			w.Write(genericOGImage())
			return
		}
		http.NotFound(w, r)
		return
	}
	s, err := getShareByToken(token)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if s.Status != "activo" {
		w.Write(genericOGImage())
		return
	}
	path := ogPath(token)
	if _, err := os.Stat(path); err != nil {
		// Lost from disk: regenerate without network (no logo download from a public request).
		log.Printf("OG image missing for %s, regenerating without logo", token)
		if err := writeOGImage(s, nil); err != nil {
			log.Printf("ERROR: %v", err)
			w.Write(genericOGImage())
			return
		}
	}
	http.ServeFile(w, r, path)
}

// writeOGImage renders and stores the share thumbnail atomically.
func writeOGImage(s *Share, logo image.Image) error {
	jpg, err := encodeOGJPEG(renderOGImage(OGParams{CompanyName: s.CompanyName, BrandColor: s.BrandColor, Logo: logo}))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(ogDir, 0o755); err != nil {
		return err
	}
	tmp := ogPath(s.Token) + ".tmp"
	if err := os.WriteFile(tmp, jpg, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, ogPath(s.Token))
}

// ===== Logo cache (admin-side downloads only) =====

var (
	logoCacheMu sync.Mutex
	logoCache   = map[string]image.Image{}
)

func loadLogo(ctx context.Context, u string) (image.Image, error) {
	if u == "" {
		return nil, nil
	}
	logoCacheMu.Lock()
	img, ok := logoCache[u]
	logoCacheMu.Unlock()
	if ok {
		return img, nil
	}
	img, _, err := fetchImage(ctx, u)
	if err != nil {
		return nil, err
	}
	logoCacheMu.Lock()
	if len(logoCache) > 64 {
		logoCache = map[string]image.Image{}
	}
	logoCache[u] = img
	logoCacheMu.Unlock()
	return img, nil
}

// ===== Admin API =====

type shareRequest struct {
	VideoID        string `json:"video_id"`
	Recipient      string `json:"recipient"`
	CompanyURL     string `json:"company_url"`
	CompanyName    string `json:"company_name"`
	CompanyLogoURL string `json:"company_logo_url"`
	BrandColor     string `json:"brand_color"`
	Password       string `json:"password"`
	RemovePassword bool   `json:"remove_password"` // PUT only: drop the password
	ExpiresInDays  int    `json:"expires_in_days"`
	ExpiresOn      string `json:"expires_on"` // YYYY-MM-DD, end of day Europe/Madrid
}

func (req *shareRequest) normalize() error {
	req.Recipient = strings.TrimSpace(req.Recipient)
	req.CompanyName = strings.Join(strings.Fields(req.CompanyName), " ")
	req.CompanyURL = strings.TrimSpace(req.CompanyURL)
	req.CompanyLogoURL = strings.TrimSpace(req.CompanyLogoURL)
	if req.CompanyURL != "" && !strings.Contains(req.CompanyURL, "://") {
		req.CompanyURL = "https://" + req.CompanyURL
	}
	for _, u := range []string{req.CompanyURL, req.CompanyLogoURL} {
		if u == "" {
			continue
		}
		if pu, err := url.Parse(u); err != nil || (pu.Scheme != "http" && pu.Scheme != "https") || pu.Host == "" {
			return fmt.Errorf("URL no válida: %s", u)
		}
	}
	if len([]rune(req.CompanyName)) > 80 {
		return errors.New("el nombre de la empresa es demasiado largo (máx 80)")
	}
	if req.BrandColor != "" {
		if req.BrandColor = normalizeHexColor(req.BrandColor); req.BrandColor == "" {
			return errors.New("color de marca no válido (usa #RRGGBB)")
		}
	}
	return nil
}

func (req *shareRequest) expiry() (*time.Time, error) {
	switch {
	case req.ExpiresOn != "":
		d, err := time.ParseInLocation("2006-01-02", req.ExpiresOn, madrid)
		if err != nil {
			return nil, errors.New("fecha de caducidad no válida")
		}
		t := d.Add(24*time.Hour - time.Second).UTC()
		if t.Before(time.Now()) {
			return nil, errors.New("la fecha de caducidad ya ha pasado")
		}
		return &t, nil
	case req.ExpiresInDays > 0:
		if req.ExpiresInDays > 3650 {
			return nil, errors.New("caducidad demasiado larga")
		}
		t := time.Now().UTC().Add(time.Duration(req.ExpiresInDays) * 24 * time.Hour)
		return &t, nil
	}
	return nil, nil
}

// fillBranding auto-detects missing company data from its website and
// downloads the logo. Errors are logged, never fatal.
func fillBranding(ctx context.Context, req *shareRequest) image.Image {
	if req.CompanyURL != "" && req.CompanyName == "" && req.CompanyLogoURL == "" {
		meta, err := analyzeCompanySite(ctx, req.CompanyURL)
		if err != nil {
			log.Printf("WARNING: could not analyze %s: %v", req.CompanyURL, err)
		} else {
			req.CompanyName, req.CompanyLogoURL = meta.Name, meta.LogoURL
			if req.BrandColor == "" {
				req.BrandColor = meta.BrandColor
			}
		}
	}
	if req.CompanyName != "" && req.BrandColor == "" {
		req.BrandColor = defaultBrandColor
	}
	logo, err := loadLogo(ctx, req.CompanyLogoURL)
	if err != nil {
		log.Printf("WARNING: could not load logo %s: %v", req.CompanyLogoURL, err)
	}
	return logo
}

// handleAdminShares: GET list / POST create on /api/admin/shares.
func handleAdminShares(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		shares, err := listShares(r)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, shares)
	case http.MethodPost:
		createShare(w, r)
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "método no permitido")
	}
}

func createShare(w http.ResponseWriter, r *http.Request) {
	var req shareRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "JSON no válido")
		return
	}
	if err := req.normalize(); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Recipient == "" || len([]rune(req.Recipient)) > 100 {
		writeJSONError(w, http.StatusBadRequest, "el destinatario es obligatorio (máx 100 caracteres)")
		return
	}
	var exists int
	db.QueryRow("SELECT COUNT(*) FROM videos WHERE video_id = ?", req.VideoID).Scan(&exists)
	if exists == 0 {
		writeJSONError(w, http.StatusBadRequest, "el vídeo no existe")
		return
	}
	expires, err := req.expiry()
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	var hash string
	if req.Password != "" {
		h, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "contraseña no válida (máx 72 bytes)")
			return
		}
		hash = string(h)
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	logo := fillBranding(ctx, &req)

	token := newToken()
	var expiresArg any
	if expires != nil {
		expiresArg = *expires
	}
	res, err := db.Exec(`INSERT INTO shares (token, video_id, recipient, company_name, company_url,
		company_logo_url, brand_color, password_hash, expires_at, created_at, revoked)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)`,
		token, req.VideoID, req.Recipient, req.CompanyName, req.CompanyURL, req.CompanyLogoURL,
		req.BrandColor, hash, expiresArg, time.Now().UTC())
	if err != nil {
		log.Printf("ERROR: Failed to create share: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "no se pudo crear el enlace")
		return
	}
	id, _ := res.LastInsertId()
	s, err := getShareByID(id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := writeOGImage(s, logo); err != nil {
		log.Printf("ERROR: Failed to write OG image for %s: %v", token, err)
	}
	decorateShare(r, s)
	log.Printf("Share created - token: %s, video: %s, recipient: %s, company: %q", token, s.VideoID, s.Recipient, s.CompanyName)
	writeJSON(w, http.StatusCreated, s)
}

// handleAdminShareByID: PUT /api/admin/shares/{id} (branding) and
// POST /api/admin/shares/{id}/revoke.
func handleAdminShareByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/admin/shares/")
	switch rest {
	case "preview":
		handleSharePreview(w, r)
		return
	case "preview.jpg":
		handleSharePreviewImage(w, r)
		return
	}
	idStr, action, _ := strings.Cut(rest, "/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "no encontrado")
		return
	}
	s, err := getShareByID(id)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "enlace no encontrado")
		return
	}
	switch {
	case action == "revoke" && r.Method == http.MethodPost:
		if _, err := db.Exec("UPDATE shares SET revoked = 1 WHERE id = ?", id); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		log.Printf("Share revoked - token: %s", s.Token)
		s, _ = getShareByID(id)
	case action == "" && r.Method == http.MethodPut:
		var req shareRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "JSON no válido")
			return
		}
		if err := req.normalize(); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		// Optional password change: remove_password clears it, a non-empty
		// password replaces it, otherwise it is left untouched.
		newHash, setHash := "", false
		switch {
		case req.RemovePassword:
			setHash = true
		case req.Password != "":
			h, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "clave no válida (máx 72 bytes)")
				return
			}
			newHash, setHash = string(h), true
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		logo := fillBranding(ctx, &req)
		if _, err := db.Exec(`UPDATE shares SET company_name = ?, company_url = ?, company_logo_url = ?, brand_color = ? WHERE id = ?`,
			req.CompanyName, req.CompanyURL, req.CompanyLogoURL, req.BrandColor, id); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if setHash {
			if _, err := db.Exec(`UPDATE shares SET password_hash = ? WHERE id = ?`, newHash, id); err != nil {
				writeJSONError(w, http.StatusInternalServerError, err.Error())
				return
			}
			log.Printf("Share password updated - token: %s, has_password: %t", s.Token, newHash != "")
		}
		s, _ = getShareByID(id)
		if err := writeOGImage(s, logo); err != nil {
			log.Printf("ERROR: Failed to write OG image for %s: %v", s.Token, err)
		}
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "método no permitido")
		return
	}
	decorateShare(r, s)
	writeJSON(w, http.StatusOK, s)
}

// handleSharePreview: POST {url} -> detected company name / logo / color.
func handleSharePreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "método no permitido")
		return
	}
	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.URL) == "" {
		writeJSONError(w, http.StatusBadRequest, "falta la URL de la empresa")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	meta, err := analyzeCompanySite(ctx, strings.TrimSpace(req.URL))
	if err != nil {
		log.Printf("Preview failed for %s: %v", req.URL, err)
		writeJSONError(w, http.StatusBadGateway, "no se pudo leer la web: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, meta)
}

// handleSharePreviewImage: GET ?name=&color=&logo= -> live JPEG preview.
func handleSharePreviewImage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	var logo image.Image
	if l := strings.TrimSpace(q.Get("logo")); l != "" {
		var err error
		if logo, err = loadLogo(ctx, l); err != nil {
			log.Printf("Preview logo failed %s: %v", l, err)
		}
	}
	color := normalizeHexColor(q.Get("color"))
	if color == "" {
		color = defaultBrandColor
	}
	jpg, err := encodeOGJPEG(renderOGImage(OGParams{CompanyName: q.Get("name"), BrandColor: color, Logo: logo}))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(jpg)
}

func decorateShare(r *http.Request, s *Share) {
	b := baseURL(r)
	s.URL = b + "/v/" + s.Token
	s.OGImageURL = "/og/" + s.Token + ".jpg" + ogVersion(s.Token)
}

func listShares(r *http.Request) ([]*Share, error) {
	rows, err := db.Query(`SELECT ` + shareColumns + `,
		(SELECT COUNT(*) FROM sessions se WHERE se.share_token = s.token),
		(SELECT COALESCE(SUM(se.total_time_watched), 0) FROM sessions se WHERE se.share_token = s.token),
		(SELECT MAX(se.entered_at) FROM sessions se WHERE se.share_token = s.token)
		FROM shares s LEFT JOIN videos v ON v.video_id = s.video_id
		ORDER BY s.created_at DESC, s.id DESC LIMIT 500`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	shares := []*Share{}
	for rows.Next() {
		var visits, watched int
		var last sql.NullString
		s, err := scanShare(rows, &visits, &watched, &last)
		if err != nil {
			log.Printf("WARNING: Failed to scan share: %v", err)
			continue
		}
		s.Visits, s.WatchedSeconds = visits, watched
		if last.Valid {
			s.LastVisit = parseSQLiteTime(last.String)
		}
		decorateShare(r, s)
		shares = append(shares, s)
	}
	return shares, rows.Err()
}

// parseSQLiteTime parses timestamps returned untyped by aggregates (MAX()).
func parseSQLiteTime(v string) *time.Time {
	v = strings.TrimSuffix(v, "Z")
	for _, f := range sqlite3.SQLiteTimestampFormats {
		if t, err := time.Parse(f, v); err == nil {
			return &t
		}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
