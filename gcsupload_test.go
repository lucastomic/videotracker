package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// fakeGCS emulates the two GCS JSON API calls the server makes.
type fakeGCS struct {
	mu       sync.Mutex
	srv      *httptest.Server
	calls    int
	last     *http.Request
	lastBody string
	objects  map[string]bool
	startErr int // if != 0, the resumable POST answers this status
}

func newFakeGCS(t *testing.T) *fakeGCS {
	f := &fakeGCS{objects: map[string]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls++
		f.last = r.Clone(r.Context())
		f.lastBody = string(body)
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, `{"error":"unauthenticated"}`, http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/upload/storage/v1/b/solnow-demos/o":
			if f.startErr != 0 {
				http.Error(w, `{"error":{"message":"forbidden"}}`, f.startErr)
				return
			}
			w.Header().Set("Location", f.srv.URL+"/upload/storage/v1/b/solnow-demos/o?uploadType=resumable&upload_id=SESSION123")
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/storage/v1/b/solnow-demos/o/"):
			name := strings.TrimPrefix(r.URL.Path, "/storage/v1/b/solnow-demos/o/")
			if !f.objects[name] {
				http.Error(w, `{"error":{"code":404}}`, http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"name": name, "size": "3221225472", "contentType": "video/mp4"})
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.String(), http.StatusTeapot)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGCS) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// useFakeGCS points the global gcs client at the fake server for one test.
func useFakeGCS(t *testing.T) *fakeGCS {
	f := newFakeGCS(t)
	prev := gcs
	gcs = &gcsClient{
		bucket:  "solnow-demos",
		prefix:  "videos/",
		apiBase: f.srv.URL,
		tokens:  oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test-token"}),
		http:    f.srv.Client(),
	}
	t.Cleanup(func() { gcs = prev })
	return f
}

func doUpload(t *testing.T, path, body, origin string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	newRouter().ServeHTTP(rec, req)
	return rec
}

func TestUploadEndpointsRequireSession(t *testing.T) {
	f := useFakeGCS(t)
	for _, p := range []string{"/api/upload/start", "/api/upload/complete"} {
		rec := doUpload(t, p, `{"filename":"a.mp4","size":10,"content_type":"video/mp4"}`, "")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("POST %s sin sesión: %d, want 401", p, rec.Code)
		}
	}
	if f.callCount() != 0 {
		t.Errorf("sin sesión no se debe llamar a GCS (%d llamadas)", f.callCount())
	}
}

var objectNameRe = regexp.MustCompile(`^videos/\d{8}-\d{6}-demo-barco-ibiza\.mp4$`)

func TestUploadStartCreatesResumableSession(t *testing.T) {
	f := useFakeGCS(t)
	t.Setenv("PUBLIC_BASE_URL", "https://demos.solnow.io/")
	rec := doUpload(t, "/api/upload/start",
		`{"filename":"Demo Barco Ibiza.MP4","size":3221225472,"content_type":"video/mp4","title":"Demo barco","video_id":"demo-barco"}`,
		"https://demos.solnow.io", adminCookie())
	if rec.Code != http.StatusOK {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct{ SessionURL, Object, PublicURL string }
	var raw map[string]string
	json.Unmarshal(rec.Body.Bytes(), &raw)
	resp.SessionURL, resp.Object, resp.PublicURL = raw["session_url"], raw["object"], raw["public_url"]

	if !objectNameRe.MatchString(resp.Object) {
		t.Errorf("nombre de objeto inesperado: %q", resp.Object)
	}
	if !strings.HasSuffix(resp.SessionURL, "upload_id=SESSION123") {
		t.Errorf("session_url: %q", resp.SessionURL)
	}
	if resp.PublicURL != "https://storage.googleapis.com/solnow-demos/"+resp.Object {
		t.Errorf("public_url: %q", resp.PublicURL)
	}

	r := f.last
	if r.Method != "POST" || r.URL.Path != "/upload/storage/v1/b/solnow-demos/o" {
		t.Fatalf("petición a GCS inesperada: %s %s", r.Method, r.URL)
	}
	q := r.URL.Query()
	if q.Get("uploadType") != "resumable" || q.Get("name") != resp.Object || q.Get("ifGenerationMatch") != "0" {
		t.Errorf("query: %v", q)
	}
	want := map[string]string{
		"Authorization":           "Bearer test-token",
		"X-Upload-Content-Type":   "video/mp4",
		"X-Upload-Content-Length": "3221225472",
		"Origin":                  "https://demos.solnow.io",
	}
	for h, v := range want {
		if got := r.Header.Get(h); got != v {
			t.Errorf("cabecera %s = %q, want %q", h, got, v)
		}
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		t.Errorf("Content-Type = %q", r.Header.Get("Content-Type"))
	}
	var meta map[string]string
	if err := json.Unmarshal([]byte(f.lastBody), &meta); err != nil {
		t.Fatalf("body no es JSON: %q", f.lastBody)
	}
	if meta["contentType"] != "video/mp4" || meta["cacheControl"] != "public, max-age=86400" {
		t.Errorf("metadatos: %v", meta)
	}
}

func TestUploadStartOrigin(t *testing.T) {
	f := useFakeGCS(t)
	// Sin PUBLIC_BASE_URL: el origen sale de X-Forwarded-Proto + Host.
	req := httptest.NewRequest("POST", "/api/upload/start", strings.NewReader(`{"filename":"x.mov","size":1000,"content_type":""}`))
	req.Host = "demos.solnow.io"
	req.Header.Set("X-Forwarded-Proto", "https")
	req.AddCookie(adminCookie())
	rec := httptest.NewRecorder()
	newRouter().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	if got := f.last.Header.Get("Origin"); got != "https://demos.solnow.io" {
		t.Errorf("Origin = %q", got)
	}
	// content_type vacío + .mov => video/quicktime
	if got := f.last.Header.Get("X-Upload-Content-Type"); got != "video/quicktime" {
		t.Errorf("X-Upload-Content-Type = %q", got)
	}

	// Un Origin del navegador distinto del público fallaría por CORS: se rechaza antes.
	t.Setenv("PUBLIC_BASE_URL", "https://demos.solnow.io")
	before := f.callCount()
	rec = doUpload(t, "/api/upload/start", `{"filename":"x.mp4","size":1000,"content_type":"video/mp4"}`, "https://evil.example", adminCookie())
	if rec.Code != http.StatusBadRequest || f.callCount() != before {
		t.Errorf("origin distinto: %d (llamadas GCS %d→%d)", rec.Code, before, f.callCount())
	}
}

func TestUploadStartValidation(t *testing.T) {
	f := useFakeGCS(t)
	c := adminCookie()
	cases := []struct {
		name, body string
		want       int
	}{
		{"no es vídeo", `{"filename":"doc.pdf","size":1000,"content_type":"application/pdf"}`, 400},
		{"imagen", `{"filename":"a.jpg","size":1000,"content_type":"image/jpeg"}`, 400},
		{"sin tipo ni extensión de vídeo", `{"filename":"notas.txt","size":1000,"content_type":""}`, 400},
		{"demasiado grande", `{"filename":"a.mp4","size":5368709121,"content_type":"video/mp4"}`, 400},
		{"vacío", `{"filename":"a.mp4","size":0,"content_type":"video/mp4"}`, 400},
		{"sin nombre", `{"filename":"","size":10,"content_type":"video/mp4"}`, 400},
		{"ID con espacios", `{"filename":"a.mp4","size":10,"content_type":"video/mp4","title":"T","video_id":"Mi vídeo"}`, 400},
		{"ID con mayúsculas", `{"filename":"a.mp4","size":10,"content_type":"video/mp4","title":"T","video_id":"Demo"}`, 400},
		{"ID duplicado", `{"filename":"a.mp4","size":10,"content_type":"video/mp4","title":"T","video_id":"demo"}`, 409},
		{"título vacío", `{"filename":"a.mp4","size":10,"content_type":"video/mp4","title":"","video_id":"nuevo"}`, 400},
		{"JSON roto", `{"filename":`, 400},
	}
	for _, tc := range cases {
		rec := doUpload(t, "/api/upload/start", tc.body, "", c)
		if rec.Code != tc.want {
			t.Errorf("%s: %d %s, want %d", tc.name, rec.Code, rec.Body.String(), tc.want)
		}
		if !strings.Contains(rec.Header().Get("Content-Type"), "application/json") {
			t.Errorf("%s: el error debe ser JSON", tc.name)
		}
	}
	if f.callCount() != 0 {
		t.Errorf("peticiones inválidas no deben llegar a GCS (%d llamadas)", f.callCount())
	}
	// Justo 5 GB sí se acepta.
	if rec := doUpload(t, "/api/upload/start", `{"filename":"a.mp4","size":5368709120,"content_type":"video/mp4"}`, "", c); rec.Code != 200 {
		t.Errorf("5 GB exactos: %d %s", rec.Code, rec.Body.String())
	}
}

func TestUploadStartGCSError(t *testing.T) {
	f := useFakeGCS(t)
	f.startErr = http.StatusForbidden
	rec := doUpload(t, "/api/upload/start", `{"filename":"a.mp4","size":10,"content_type":"video/mp4"}`, "", adminCookie())
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "403") {
		t.Errorf("error de GCS: %d %s", rec.Code, rec.Body.String())
	}
}

func TestUploadComplete(t *testing.T) {
	f := useFakeGCS(t)
	c := adminCookie()
	obj := "videos/20261125-101500-demo-barco-ibiza.mp4"

	// Objeto que no existe (la subida no terminó): 404 y no se crea el vídeo.
	rec := doUpload(t, "/api/upload/complete", `{"object":"`+obj+`","title":"Demo barco Ibiza","video_id":"demo-barco-ibiza"}`, "", c)
	if rec.Code != http.StatusNotFound {
		t.Errorf("objeto inexistente: %d %s", rec.Code, rec.Body.String())
	}
	if videoIDExists("demo-barco-ibiza") {
		t.Fatal("se creó el vídeo sin objeto en GCS")
	}
	if got := f.last.URL.EscapedPath(); got != "/storage/v1/b/solnow-demos/o/videos%2F20261125-101500-demo-barco-ibiza.mp4" {
		t.Errorf("ruta de metadatos: %q", got)
	}
	if f.last.Header.Get("Authorization") != "Bearer test-token" {
		t.Errorf("metadatos sin token")
	}

	// Existe: crea el vídeo con la URL pública.
	f.objects[obj] = true
	rec = doUpload(t, "/api/upload/complete", `{"object":"`+obj+`","title":"Demo barco Ibiza","video_id":"demo-barco-ibiza"}`, "", c)
	if rec.Code != http.StatusCreated {
		t.Fatalf("complete: %d %s", rec.Code, rec.Body.String())
	}
	var v Video
	json.Unmarshal(rec.Body.Bytes(), &v)
	wantURL := "https://storage.googleapis.com/solnow-demos/" + obj
	if v.VideoID != "demo-barco-ibiza" || v.Title != "Demo barco Ibiza" || v.URL != wantURL {
		t.Errorf("vídeo devuelto: %+v", v)
	}
	var dbURL string
	db.QueryRow("SELECT url FROM videos WHERE video_id = ?", "demo-barco-ibiza").Scan(&dbURL)
	if dbURL != wantURL {
		t.Errorf("url en BD: %q", dbURL)
	}
	// Ya se puede crear un enlace para compartir con ese vídeo.
	if rec := do(t, "POST", "/api/admin/shares", `{"video_id":"demo-barco-ibiza","recipient":"cliente-x"}`, c); rec.Code != http.StatusCreated {
		t.Errorf("enlace con el vídeo subido: %d %s", rec.Code, rec.Body.String())
	}

	// Mismo ID otra vez: 409.
	if rec := doUpload(t, "/api/upload/complete", `{"object":"`+obj+`","title":"Otra","video_id":"demo-barco-ibiza"}`, "", c); rec.Code != http.StatusConflict {
		t.Errorf("ID duplicado: %d", rec.Code)
	}

	// Objetos fuera del prefijo o con nombres raros no se aceptan (ni se consultan).
	before := f.callCount()
	for _, bad := range []string{"otros/20261125-101500-x.mp4", "videos/../secreto.mp4", "videos/x.mp4", ""} {
		if rec := doUpload(t, "/api/upload/complete", `{"object":"`+bad+`","title":"T","video_id":"bad-obj"}`, "", c); rec.Code != http.StatusBadRequest {
			t.Errorf("objeto %q: %d", bad, rec.Code)
		}
	}
	// ID / título no válidos.
	if rec := doUpload(t, "/api/upload/complete", `{"object":"`+obj+`","title":"T","video_id":"con espacio"}`, "", c); rec.Code != http.StatusBadRequest {
		t.Errorf("ID no válido: %d", rec.Code)
	}
	if rec := doUpload(t, "/api/upload/complete", `{"object":"`+obj+`","title":"  ","video_id":"sin-titulo"}`, "", c); rec.Code != http.StatusBadRequest {
		t.Errorf("título vacío: %d", rec.Code)
	}
	if f.callCount() != before {
		t.Errorf("validaciones fallidas no deben consultar GCS")
	}
}

func TestUploadDisabledWithoutBucket(t *testing.T) {
	prev := gcs
	t.Cleanup(func() { gcs = prev })
	t.Setenv("GCS_BUCKET", "")
	configureGCS()
	if gcs != nil {
		t.Fatal("sin GCS_BUCKET la subida debe estar desactivada")
	}
	c := adminCookie()
	for _, p := range []string{"/api/upload/start", "/api/upload/complete"} {
		if rec := doUpload(t, p, `{}`, "", c); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "GCS_BUCKET") {
			t.Errorf("%s sin bucket: %d %s", p, rec.Code, rec.Body.String())
		}
	}
	rec := do(t, "GET", "/admin", "", c)
	body := rec.Body.String()
	if !strings.Contains(body, "Subida de vídeos desactivada") || strings.Contains(body, `id="upload-file-input"`) ||
		!regexp.MustCompile(`var GCS_ENABLED =\s*false\s*;`).MatchString(body) {
		t.Errorf("el admin debe mostrar la zona de subida desactivada")
	}
}

func TestConfigureGCSPrefix(t *testing.T) {
	prev := gcs
	t.Cleanup(func() { gcs = prev })
	t.Setenv("GCS_BUCKET", "solnow-demos")
	t.Setenv("GCS_PREFIX", "")
	configureGCS()
	if gcs == nil || gcs.bucket != "solnow-demos" || gcs.prefix != "videos/" {
		t.Fatalf("config por defecto: %+v", gcs)
	}
	t.Setenv("GCS_PREFIX", "/demos/2026")
	configureGCS()
	if gcs.prefix != "demos/2026/" {
		t.Errorf("prefijo normalizado: %q", gcs.prefix)
	}
}

func TestAdminRendersDropzone(t *testing.T) {
	useFakeGCS(t)
	rec := do(t, "GET", "/admin", "", adminCookie())
	body := rec.Body.String()
	for _, want := range []string{"Arrastra aquí un vídeo o haz clic para elegirlo", `id="upload-file-input"`, "solnow-demos"} {
		if !strings.Contains(body, want) {
			t.Errorf("el admin no contiene %q", want)
		}
	}
	if !regexp.MustCompile(`var GCS_ENABLED =\s*true\s*;`).MatchString(body) {
		t.Errorf("GCS_ENABLED no es true en el JS")
	}
	if strings.Contains(body, "presign") || strings.Contains(body, "S3Enabled") {
		t.Errorf("quedan restos de la subida a S3 en el admin")
	}
}

func TestUploadObjectName(t *testing.T) {
	now := time.Date(2026, 11, 25, 9, 5, 7, 0, time.UTC) // 10:05:07 en Madrid
	cases := []struct{ file, ct, want string }{
		{"Demo Ñandú — Cala d'Or (final).MOV", "video/quicktime", "videos/20261125-100507-demo-nandu-cala-d-or-final.mov"},
		{"  ...mp4", "video/mp4", "videos/20261125-100507-video.mp4"},
		{"sin_extension", "video/webm", "videos/20261125-100507-sin-extension.webm"},
		{"雪.mkv", "video/x-matroska", "videos/20261125-100507-video.mkv"},
	}
	for _, tc := range cases {
		got := uploadObjectName("videos/", tc.file, tc.ct, now)
		if got != tc.want {
			t.Errorf("uploadObjectName(%q) = %q, want %q", tc.file, got, tc.want)
		}
		g := &gcsClient{prefix: "videos/"}
		if !g.validObject(got) {
			t.Errorf("%q debería ser un objeto válido", got)
		}
	}
	if s := slugify(strings.Repeat("a", 100), 60); len(s) != 60 {
		t.Errorf("slug sin límite: %d", len(s))
	}
}
