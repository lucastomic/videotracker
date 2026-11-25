package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"html/template"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testVideoURL = "https://storage.example.com/videos/secret-demo-video.mp4"

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "vt-test-*")
	if err != nil {
		panic(err)
	}
	db, err = sql.Open("sqlite3", filepath.Join(dir, "test.db"))
	if err != nil {
		panic(err)
	}
	createTables()
	createTables() // migrations must be idempotent
	ogDir = filepath.Join(dir, "og")
	os.Setenv("ADMIN_PASSWORD", "test")
	os.Unsetenv("SESSION_SECRET")
	os.Unsetenv("PUBLIC_BASE_URL")
	configureAuth()
	templates = template.Must(template.ParseGlob("templates/*.html"))
	if _, err := db.Exec("INSERT INTO videos (video_id, title, url) VALUES (?, ?, ?)", "demo", "Demo SolNow para chárter", testVideoURL); err != nil {
		panic(err)
	}
	code := m.Run()
	db.Close()
	os.RemoveAll(dir)
	os.Exit(code)
}

func adminCookie() *http.Cookie {
	return &http.Cookie{Name: adminCookieName, Value: makeSignedValue("admin", time.Now().Add(time.Hour))}
}

func do(t *testing.T, method, path string, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != "" && strings.HasPrefix(body, "{") {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	req.Header.Set("User-Agent", "Mozilla/5.0 test")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	newRouter().ServeHTTP(rec, req)
	return rec
}

// ===== Auth =====

func TestAdminRoutesRequireSession(t *testing.T) {
	apis := []struct{ method, path string }{
		{"GET", "/api/videos"}, {"POST", "/api/videos"}, {"GET", "/api/videos/demo"},
		{"PUT", "/api/videos/demo"}, {"DELETE", "/api/videos/demo"}, {"GET", "/api/admin/data"},
		{"GET", "/api/admin/shares"}, {"POST", "/api/admin/shares"}, {"POST", "/api/admin/shares/preview"},
		{"GET", "/api/admin/shares/preview.jpg"}, {"POST", "/api/admin/shares/1/revoke"},
		{"GET", "/api/admin/whatever"}, {"POST", "/api/upload/presign"},
	}
	for _, a := range apis {
		rec := do(t, a.method, a.path, "")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without session: got %d, want 401", a.method, a.path, rec.Code)
		}
		if !strings.Contains(rec.Header().Get("Content-Type"), "application/json") {
			t.Errorf("%s %s: 401 must be JSON", a.method, a.path)
		}
	}
	rec := do(t, "GET", "/admin", "")
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/admin/login") {
		t.Errorf("/admin without session: got %d %q, want redirect to login", rec.Code, rec.Header().Get("Location"))
	}
	// Forged / expired cookies are rejected.
	bad := &http.Cookie{Name: adminCookieName, Value: "9999999999.forged"}
	if rec := do(t, "GET", "/api/admin/data", "", bad); rec.Code != http.StatusUnauthorized {
		t.Errorf("forged cookie accepted: %d", rec.Code)
	}
	expired := &http.Cookie{Name: adminCookieName, Value: makeSignedValue("admin", time.Now().Add(-time.Minute))}
	if rec := do(t, "GET", "/api/admin/data", "", expired); rec.Code != http.StatusUnauthorized {
		t.Errorf("expired cookie accepted: %d", rec.Code)
	}
}

func TestAdminRoutesWithSession(t *testing.T) {
	c := adminCookie()
	if rec := do(t, "GET", "/admin", "", c); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Enlaces para compartir") {
		t.Errorf("/admin with session: %d", rec.Code)
	}
	for _, p := range []string{"/api/videos", "/api/admin/data", "/api/admin/shares", "/api/videos/demo"} {
		if rec := do(t, "GET", p, "", c); rec.Code != http.StatusOK {
			t.Errorf("GET %s with session: %d %s", p, rec.Code, rec.Body.String())
		}
	}
}

func TestPublicRoutesStayPublic(t *testing.T) {
	if rec := do(t, "GET", "/", ""); rec.Code != http.StatusOK {
		t.Errorf("/: %d", rec.Code)
	}
	rec := do(t, "GET", "/watch?video_id=demo&user_id=cliente1", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), testVideoURL) {
		t.Errorf("legacy /watch link broken: %d", rec.Code)
	}
	if rec := do(t, "GET", "/static/images/logo.png", ""); rec.Code != http.StatusOK {
		t.Errorf("static: %d", rec.Code)
	}
}

func TestLoginFlow(t *testing.T) {
	loginLimiter.reset("203.0.113.7")
	rec := do(t, "POST", "/admin/login", "password=wrong")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", rec.Code)
	}
	rec = do(t, "POST", "/admin/login", "password=test&next=/admin")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/admin" {
		t.Fatalf("login: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	var session *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == adminCookieName {
			session = c
		}
	}
	if session == nil || !session.HttpOnly || !session.Secure || session.SameSite != http.SameSiteLaxMode || session.MaxAge < 29*24*3600 {
		t.Fatalf("bad session cookie: %+v", session)
	}
	if rec := do(t, "GET", "/api/admin/data", "", session); rec.Code != http.StatusOK {
		t.Errorf("session from login rejected: %d", rec.Code)
	}
	// Open redirect protection.
	rec = do(t, "POST", "/admin/login", "password=test&next=https://evil.example")
	if rec.Header().Get("Location") != "/admin" {
		t.Errorf("open redirect: %s", rec.Header().Get("Location"))
	}
}

func TestLoginRateLimit(t *testing.T) {
	loginLimiter.reset("203.0.113.7")
	defer loginLimiter.reset("203.0.113.7")
	for i := 0; i < maxFailedAttempts; i++ {
		do(t, "POST", "/admin/login", "password=nope")
	}
	// Even the right password is refused while blocked.
	if rec := do(t, "POST", "/admin/login", "password=test"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 after %d failures, got %d", maxFailedAttempts, rec.Code)
	}
}

func TestAdminDisabledWithoutPassword(t *testing.T) {
	old := adminPassword
	adminPassword = ""
	defer func() { adminPassword = old }()
	for _, p := range []string{"/admin", "/admin/login", "/api/admin/data"} {
		if rec := do(t, "GET", p, "", adminCookie()); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "ADMIN_PASSWORD no configurada") {
			t.Errorf("%s without ADMIN_PASSWORD: %d %s", p, rec.Code, rec.Body.String())
		}
	}
}

// ===== Shares =====

func createTestShare(t *testing.T, body string) Share {
	t.Helper()
	rec := do(t, "POST", "/api/admin/shares", body, adminCookie())
	if rec.Code != http.StatusCreated {
		t.Fatalf("create share: %d %s", rec.Code, rec.Body.String())
	}
	var s Share
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Token) < 10 {
		t.Fatalf("token too short: %q", s.Token)
	}
	return s
}

func assertOGTags(t *testing.T, body string) {
	t.Helper()
	for _, tag := range []string{`property="og:title"`, `property="og:description"`, `property="og:image"`,
		`property="og:image:width" content="1200"`, `property="og:image:height" content="630"`,
		`property="og:url"`, `name="twitter:card" content="summary_large_image"`} {
		if !strings.Contains(body, tag) {
			t.Errorf("missing OG tag %s", tag)
		}
	}
}

func TestSharePublicFlow(t *testing.T) {
	s := createTestShare(t, `{"video_id":"demo","recipient":"marina-test","company_name":"Marina Test","brand_color":"#1a5f7a","expires_in_days":30}`)
	rec := do(t, "GET", "/v/"+s.Token, "")
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, testVideoURL) {
		t.Fatalf("share page: %d", rec.Code)
	}
	assertOGTags(t, body)
	if !strings.Contains(body, "Marina Test, tu demo de SolNow") || !strings.Contains(body, "Demo SolNow para chárter · Mira cómo gestionar") {
		t.Errorf("OG texts wrong")
	}
	if !strings.Contains(body, "/og/"+s.Token+".jpg") {
		t.Errorf("og:image should point to the share thumbnail")
	}
	// Session recorded with recipient as user_id and the share token.
	var user, tok string
	if err := db.QueryRow("SELECT user_id, share_token FROM sessions WHERE share_token = ? ORDER BY id DESC", s.Token).Scan(&user, &tok); err != nil || user != "marina-test" {
		t.Errorf("session not recorded: %v %q", err, user)
	}
	// Preview bots don't create sessions.
	var before, after int
	db.QueryRow("SELECT COUNT(*) FROM sessions WHERE share_token = ?", s.Token).Scan(&before)
	req := httptest.NewRequest("GET", "/v/"+s.Token, nil)
	req.Header.Set("User-Agent", "WhatsApp/2.23.20.0 A")
	rr := httptest.NewRecorder()
	newRouter().ServeHTTP(rr, req)
	db.QueryRow("SELECT COUNT(*) FROM sessions WHERE share_token = ?", s.Token).Scan(&after)
	if rr.Code != http.StatusOK || after != before {
		t.Errorf("bot fetch: code %d, sessions %d -> %d", rr.Code, before, after)
	}
	// Thumbnail served, cacheable, valid size.
	img := do(t, "GET", "/og/"+s.Token+".jpg", "")
	if img.Code != http.StatusOK || img.Header().Get("Content-Type") != "image/jpeg" || !strings.Contains(img.Header().Get("Cache-Control"), "max-age=86400") {
		t.Errorf("og image: %d %v", img.Code, img.Header())
	}
	// Stats in the admin list.
	list := do(t, "GET", "/api/admin/shares", "", adminCookie())
	var shares []Share
	json.Unmarshal(list.Body.Bytes(), &shares)
	found := false
	for _, sh := range shares {
		if sh.Token == s.Token {
			found = true
			if sh.Visits < 1 || sh.Status != "activo" || sh.LastVisit == nil {
				t.Errorf("bad stats: %+v", sh)
			}
		}
	}
	if !found {
		t.Errorf("share not listed")
	}
	// Revoke -> 410.
	if rec := do(t, "POST", "/api/admin/shares/"+itoa(s.ID)+"/revoke", "", adminCookie()); rec.Code != http.StatusOK {
		t.Fatalf("revoke: %d", rec.Code)
	}
	if rec := do(t, "GET", "/v/"+s.Token, ""); rec.Code != http.StatusGone || strings.Contains(rec.Body.String(), testVideoURL) {
		t.Errorf("revoked share: %d", rec.Code)
	}
}

func itoa(i int64) string { return strconv.FormatInt(i, 10) }

func TestShareExpired(t *testing.T) {
	token := "expiredtoken123"
	_, err := db.Exec(`INSERT INTO shares (token, video_id, recipient, company_name, expires_at, created_at) VALUES (?, 'demo', 'x', 'Empresa X', ?, ?)`,
		token, time.Now().Add(-time.Hour).UTC(), time.Now().Add(-48*time.Hour).UTC())
	if err != nil {
		t.Fatal(err)
	}
	rec := do(t, "GET", "/v/"+token, "")
	body := rec.Body.String()
	if rec.Code != http.StatusGone {
		t.Errorf("expired: got %d, want 410", rec.Code)
	}
	if !strings.Contains(body, "Este enlace ha caducado") || strings.Contains(body, testVideoURL) || strings.Contains(body, "<video") {
		t.Errorf("expired page content wrong")
	}
	assertOGTags(t, body)
	if strings.Contains(body, "Empresa X") || !strings.Contains(body, "/og/solnow.jpg") {
		t.Errorf("expired page must use generic OG")
	}
	if rec := do(t, "GET", "/v/doesnotexist000", ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown token: %d", rec.Code)
	}
}

func TestSharePassword(t *testing.T) {
	shareLimiter.reset("203.0.113.7")
	s := createTestShare(t, `{"video_id":"demo","recipient":"privado","company_name":"Náutica Privada","password":"barco123"}`)
	if !s.HasPassword {
		t.Fatal("has_password should be true")
	}
	var hash string
	db.QueryRow("SELECT password_hash FROM shares WHERE id = ?", s.ID).Scan(&hash)
	if !strings.HasPrefix(hash, "$2") {
		t.Errorf("password not bcrypt-hashed: %q", hash)
	}

	// No cookie: password form, OG tags present, no video URL anywhere.
	rec := do(t, "GET", "/v/"+s.Token, "")
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `type="password"`) {
		t.Fatalf("password page: %d", rec.Code)
	}
	assertOGTags(t, body)
	if !strings.Contains(body, "Náutica Privada, tu demo de SolNow") {
		t.Errorf("OG title missing on password page")
	}
	if strings.Contains(body, testVideoURL) || strings.Contains(body, "secret-demo-video") || strings.Contains(body, "<video") {
		t.Errorf("video URL leaked before password")
	}

	// Wrong password.
	rec = do(t, "POST", "/v/"+s.Token, "password=nope")
	if rec.Code != http.StatusUnauthorized || strings.Contains(rec.Body.String(), testVideoURL) {
		t.Errorf("wrong password: %d", rec.Code)
	}
	assertOGTags(t, rec.Body.String())

	// Right password -> cookie -> video.
	rec = do(t, "POST", "/v/"+s.Token, "password=barco123")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("right password: %d", rec.Code)
	}
	var c *http.Cookie
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == shareCookieName(s.Token) {
			c = ck
		}
	}
	if c == nil || !c.HttpOnly || !c.Secure || c.Path != "/v/"+s.Token {
		t.Fatalf("bad share cookie: %+v", c)
	}
	rec = do(t, "GET", "/v/"+s.Token, "", c)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), testVideoURL) {
		t.Errorf("video not shown after password: %d", rec.Code)
	}
	// The cookie of one share does not unlock another one.
	other := createTestShare(t, `{"video_id":"demo","recipient":"otro","password":"barco123"}`)
	stolen := &http.Cookie{Name: shareCookieName(other.Token), Value: c.Value}
	if rec := do(t, "GET", "/v/"+other.Token, "", stolen); strings.Contains(rec.Body.String(), testVideoURL) {
		t.Errorf("cookie reused across shares")
	}

	// Rate limit.
	shareLimiter.reset("203.0.113.7")
	defer shareLimiter.reset("203.0.113.7")
	for i := 0; i < maxFailedAttempts; i++ {
		do(t, "POST", "/v/"+s.Token, "password=x")
	}
	if rec := do(t, "POST", "/v/"+s.Token, "password=barco123"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("share rate limit: %d", rec.Code)
	}
}

func TestShareValidation(t *testing.T) {
	c := adminCookie()
	for _, body := range []string{
		`{"video_id":"nope","recipient":"a"}`,
		`{"video_id":"demo","recipient":""}`,
		`{"video_id":"demo","recipient":"a","brand_color":"blue"}`,
		`{"video_id":"demo","recipient":"a","expires_on":"2001-01-01"}`,
		`{"video_id":"demo","recipient":"a","company_logo_url":"file:///etc/passwd"}`,
	} {
		if rec := do(t, "POST", "/api/admin/shares", body, c); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", body, rec.Code)
		}
	}
}

// ===== SSRF =====

func TestSSRFBlocked(t *testing.T) {
	for _, u := range []string{
		"http://127.0.0.1/", "http://127.0.0.1:8080/admin", "http://localhost/",
		"http://169.254.169.254/latest/meta-data/", "http://metadata.google.internal/computeMetadata/v1/",
		"http://10.0.0.1/", "http://10.1.2.3:9000/", "http://192.168.1.1/", "http://172.16.0.5/",
		"http://[::1]/", "http://[fd00:ec2::254]/", "http://[::ffff:127.0.0.1]/", "http://0.0.0.0/",
		"http://100.64.0.1/", "file:///etc/passwd", "gopher://127.0.0.1/", "ftp://example.com/",
	} {
		_, _, _, err := safeFetch(context.Background(), u)
		if err == nil {
			t.Errorf("%s: fetch should be blocked", u)
		}
	}
	// A real local server is unreachable through the safe client too.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("secret")) }))
	defer srv.Close()
	if _, _, _, err := safeFetch(context.Background(), srv.URL); err == nil || !errors.Is(err, errBlockedAddress) {
		t.Errorf("local httptest server reachable: %v", err)
	}
	for _, ip := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if isBlockedIP(netip.MustParseAddr(ip)) {
			t.Errorf("%s should be allowed", ip)
		}
	}
	// Redirects are re-checked.
	if err := safeHTTPClient.CheckRedirect(&http.Request{URL: mustURL("http://169.254.169.254/")}, []*http.Request{{}}); err == nil {
		t.Errorf("redirect to metadata IP allowed")
	}
	if err := safeHTTPClient.CheckRedirect(&http.Request{URL: mustURL("https://example.com/")}, make([]*http.Request, fetchMaxRedirects)); err == nil {
		t.Errorf("more than %d redirects allowed", fetchMaxRedirects)
	}
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

// ===== Metadata extraction =====

func TestExtractMetadata(t *testing.T) {
	page := `<!doctype html><html><head>
	<title>Alquiler de barcos en Ibiza | Marina Ibiza</title>
	<meta property="og:title" content="Marina Ibiza - Puerto deportivo de lujo">
	<meta name="theme-color" content="#0A4C8B">
	<meta property="og:image" content="/img/og-photo.jpg">
	<link rel="icon" href="/favicon-32.png" sizes="32x32">
	<link rel="icon" href="/favicon.svg" type="image/svg+xml">
	<link rel="icon" href="/icon-192.png" sizes="192x192">
	<link rel="apple-touch-icon" href="/apple-touch-icon.png" sizes="180x180">
	</head><body><header><a href="/"><img src="logo-header.png" class="site-logo" alt="Marina Ibiza"></a></header></body></html>`
	m := extractMetadata([]byte(page), mustURL("https://www.marinaibiza.com/es/"))
	if m.Name != "Marina Ibiza" {
		t.Errorf("name: %q", m.Name)
	}
	if m.ThemeColor != "#0A4C8B" {
		t.Errorf("theme color: %q", m.ThemeColor)
	}
	want := []string{
		"https://www.marinaibiza.com/apple-touch-icon.png",
		"https://www.marinaibiza.com/icon-192.png",
		"https://www.marinaibiza.com/es/logo-header.png",
		"https://www.marinaibiza.com/img/og-photo.jpg",
		"https://www.marinaibiza.com/favicon-32.png",
	}
	if len(m.LogoCandidates) < len(want) {
		t.Fatalf("candidates: %v", m.LogoCandidates)
	}
	for i, w := range want {
		if m.LogoCandidates[i] != w {
			t.Errorf("candidate %d: %q, want %q", i, m.LogoCandidates[i], w)
		}
	}
	for _, c := range m.LogoCandidates {
		if strings.HasSuffix(c, ".svg") {
			t.Errorf("svg candidate kept: %s", c)
		}
	}

	// og:site_name wins; HTML entities cleaned.
	m = extractMetadata([]byte(`<html><head><meta property="og:site_name" content="  Click&amp;Boat  "><title>Whatever</title></head></html>`), mustURL("https://www.clickandboat.com/"))
	if m.Name != "Click&Boat" {
		t.Errorf("site_name: %q", m.Name)
	}
	// Brand part of the title is chosen by matching the domain.
	m = extractMetadata([]byte(`<html><head><title>Alquiler de barcos sin licencia — Click&amp;Boat</title></head></html>`), mustURL("https://www.clickandboat.com/"))
	if m.Name != "Click&Boat" {
		t.Errorf("title brand: %q", m.Name)
	}
	// <base href> is honoured.
	m = extractMetadata([]byte(`<html><head><base href="https://cdn.example.org/site/"><link rel="apple-touch-icon" href="touch.png"></head></html>`), mustURL("https://example.org/"))
	if m.LogoURL != "https://cdn.example.org/site/touch.png" {
		t.Errorf("base href: %q", m.LogoURL)
	}
}

func TestColorHelpers(t *testing.T) {
	cases := map[string]string{"#abc": "#AABBCC", "#0b2545": "#0B2545", "rgb(10, 20, 30)": "#0A141E", "blue": "", "#12345": ""}
	for in, want := range cases {
		if got := normalizeHexColor(in); got != want {
			t.Errorf("normalizeHexColor(%q) = %q, want %q", in, got, want)
		}
	}
	white, _ := parseColor("#ffffff")
	if usableBrandColor(white) {
		t.Errorf("white must not be a brand color")
	}
	// Dominant color of a red logo on white.
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			c := color.RGBA{255, 255, 255, 255}
			if x > 16 && x < 48 {
				c = color.RGBA{200, 30, 40, 255}
			}
			img.Set(x, y, c)
		}
	}
	if c, ok := dominantColor(img); !ok || c.R < 180 || c.G > 60 {
		t.Errorf("dominant color: %v %v", c, ok)
	}
}

// ===== OG image =====

func TestOGImageSizeAndDimensions(t *testing.T) {
	logo := image.NewRGBA(image.Rect(0, 0, 180, 180))
	for y := 0; y < 180; y++ {
		for x := 0; x < 180; x++ {
			logo.Set(x, y, color.RGBA{uint8(x), 80, uint8(y), 255})
		}
	}
	cases := []OGParams{
		{}, // generic
		{CompanyName: "Marina Ibiza", BrandColor: "#0A4C8B", Logo: logo},
		{CompanyName: "Alquiler de Barcos y Veleros del Mediterráneo Occidental Sociedad Limitada Unipersonal", BrandColor: "#F5E6A8"},
		{CompanyName: "Náutica", BrandColor: "#FFFFFF", Logo: logo},
	}
	for i, p := range cases {
		jpg, err := encodeOGJPEG(renderOGImage(p))
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if len(jpg) >= ogMaxBytes {
			t.Errorf("case %d: %d bytes >= 300 KB", i, len(jpg))
		}
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(jpg))
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if cfg.Width != 1200 || cfg.Height != 630 {
			t.Errorf("case %d: %dx%d", i, cfg.Width, cfg.Height)
		}
	}
	// Stored file of a share too.
	s := createTestShare(t, `{"video_id":"demo","recipient":"img","company_name":"Velas Mallorca","brand_color":"#C0392B"}`)
	fi, err := os.Stat(ogPath(s.Token))
	if err != nil || fi.Size() >= ogMaxBytes {
		t.Errorf("stored og image: %v %v", fi, err)
	}
}

func TestTextFitting(t *testing.T) {
	loadFonts()
	size, lines := fitTitle("Marina", 674)
	if size != 84 || len(lines) != 1 {
		t.Errorf("short name: %v %v", size, lines)
	}
	size, lines = fitTitle(strings.Repeat("Palabra ", 30), 674)
	if len(lines) > 2 || !strings.HasSuffix(lines[len(lines)-1], "…") {
		t.Errorf("long name should be truncated: %v %v", size, lines)
	}
}
