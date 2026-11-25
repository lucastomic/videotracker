package main

// Direct browser -> Google Cloud Storage uploads (resumable uploads).
//
// The server never sees the video bytes: it only opens a resumable upload
// session with its own credentials (Workload Identity / ADC) and hands the
// session URL to the browser, which PUTs the file in chunks straight to GCS.
// No service-account keys and no signBlob are needed.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const (
	gcsScope        = "https://www.googleapis.com/auth/devstorage.read_write"
	gcsPublicHost   = "https://storage.googleapis.com"
	maxUploadBytes  = 5 << 30 // 5 GB
	gcsCacheControl = "public, max-age=86400"
)

// gcsClient talks to the GCS JSON API. Every external dependency (API base
// URL, token source, HTTP client) is a field so tests can use a fake server.
type gcsClient struct {
	bucket  string
	prefix  string // e.g. "videos/"
	apiBase string // https://storage.googleapis.com (overridable in tests)
	tokens  oauth2.TokenSource
	http    *http.Client
}

// gcs is nil when GCS_BUCKET is not configured (upload disabled).
var gcs *gcsClient

func configureGCS() {
	bucket := strings.TrimSpace(os.Getenv("GCS_BUCKET"))
	if bucket == "" {
		gcs = nil
		log.Println("Subida a GCS desactivada: falta GCS_BUCKET")
		return
	}
	prefix := os.Getenv("GCS_PREFIX")
	if prefix == "" {
		prefix = "videos/"
	}
	prefix = strings.TrimLeft(prefix, "/")
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	gcs = &gcsClient{
		bucket:  bucket,
		prefix:  prefix,
		apiBase: gcsPublicHost,
		tokens:  &lazyDefaultTokenSource{},
		http:    &http.Client{Timeout: 30 * time.Second},
	}
	log.Printf("Subida a GCS activada - bucket: %s, prefijo: %s", bucket, prefix)
}

// lazyDefaultTokenSource resolves Application Default Credentials on first
// use, so the app still starts (and the rest of the admin works) when they
// are missing, e.g. in local development.
type lazyDefaultTokenSource struct {
	mu sync.Mutex
	ts oauth2.TokenSource
}

func (l *lazyDefaultTokenSource) Token() (*oauth2.Token, error) {
	l.mu.Lock()
	if l.ts == nil {
		ts, err := google.DefaultTokenSource(context.Background(), gcsScope)
		if err != nil {
			l.mu.Unlock()
			return nil, fmt.Errorf("credenciales de Google no disponibles: %w", err)
		}
		l.ts = ts
	}
	ts := l.ts
	l.mu.Unlock()
	return ts.Token()
}

func (c *gcsClient) publicURL(object string) string {
	parts := strings.Split(object, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return gcsPublicHost + "/" + url.PathEscape(c.bucket) + "/" + strings.Join(parts, "/")
}

func (c *gcsClient) authorize(req *http.Request) error {
	tok, err := c.tokens.Token()
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	return nil
}

func readErrBody(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 2048))
	return strings.TrimSpace(string(b))
}

// startResumable opens a resumable upload session bound to the browser's
// origin (GCS then answers the browser's PUTs with the bucket's CORS headers).
func (c *gcsClient) startResumable(ctx context.Context, object, contentType string, size int64, origin string) (string, error) {
	body, _ := json.Marshal(map[string]string{"contentType": contentType, "cacheControl": gcsCacheControl})
	u := c.apiBase + "/upload/storage/v1/b/" + url.PathEscape(c.bucket) + "/o?uploadType=resumable" +
		"&ifGenerationMatch=0&name=" + url.QueryEscape(object)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	if err := c.authorize(req); err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("X-Upload-Content-Type", contentType)
	req.Header.Set("X-Upload-Content-Length", fmt.Sprint(size))
	req.Header.Set("Origin", origin)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("GCS respondió %d: %s", resp.StatusCode, readErrBody(resp.Body))
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", errors.New("GCS no devolvió la URL de sesión (Location)")
	}
	return loc, nil
}

type gcsObject struct {
	Name        string `json:"name"`
	Size        string `json:"size"`
	ContentType string `json:"contentType"`
}

var errObjectNotFound = errors.New("objeto no encontrado")

func (c *gcsClient) objectMetadata(ctx context.Context, object string) (*gcsObject, error) {
	u := c.apiBase + "/storage/v1/b/" + url.PathEscape(c.bucket) + "/o/" + url.PathEscape(object)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if err := c.authorize(req); err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, errObjectNotFound
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("GCS respondió %d: %s", resp.StatusCode, readErrBody(resp.Body))
	}
	var obj gcsObject
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&obj); err != nil {
		return nil, fmt.Errorf("respuesta de GCS no válida: %w", err)
	}
	return &obj, nil
}

// ===== Naming / validation =====

var (
	videoIDRegex = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,79}$`)
	accentFold   = strings.NewReplacer(
		"á", "a", "à", "a", "ä", "a", "â", "a", "ã", "a", "å", "a",
		"é", "e", "è", "e", "ë", "e", "ê", "e",
		"í", "i", "ì", "i", "ï", "i", "î", "i",
		"ó", "o", "ò", "o", "ö", "o", "ô", "o", "õ", "o", "ø", "o",
		"ú", "u", "ù", "u", "ü", "u", "û", "u",
		"ñ", "n", "ç", "c", "ß", "ss", "æ", "ae", "œ", "oe",
	)
	videoExtByType = map[string]string{
		"video/mp4": "mp4", "video/quicktime": "mov", "video/webm": "webm", "video/x-matroska": "mkv",
		"video/x-m4v": "m4v", "video/x-msvideo": "avi", "video/mpeg": "mpg", "video/ogg": "ogv",
	}
)

// videoTypeByExt guesses a content type from a file extension (".mov").
func videoTypeByExt(ext string) string {
	ext = strings.ToLower(strings.TrimPrefix(ext, "."))
	if ext == "" {
		return ""
	}
	for t, e := range videoExtByType {
		if e == ext {
			return t
		}
	}
	switch ext {
	case "mpeg":
		return "video/mpeg"
	}
	ct, _, _ := mime.ParseMediaType(mime.TypeByExtension("." + ext))
	return ct
}

// slugify returns an ASCII slug [a-z0-9-] (max maxLen chars), "" if nothing is left.
func slugify(s string, maxLen int) string {
	s = accentFold.Replace(strings.ToLower(s))
	var b strings.Builder
	dash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
		if b.Len() >= maxLen {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}

// uploadObjectName builds "<prefix><YYYYMMDD-HHMMSS>-<slug>.<ext>".
func uploadObjectName(prefix, filename, contentType string, now time.Time) string {
	ext := strings.ToLower(strings.TrimPrefix(path.Ext(filename), "."))
	base := strings.TrimSuffix(filename, path.Ext(filename))
	ext = slugify(ext, 10)
	ext = strings.ReplaceAll(ext, "-", "")
	if ext == "" {
		ext = videoExtByType[contentType]
		if ext == "" {
			ext = "bin"
		}
	}
	slug := slugify(base, 60)
	if slug == "" {
		slug = "video"
	}
	return prefix + now.In(madrid).Format("20060102-150405") + "-" + slug + "." + ext
}

func (c *gcsClient) validObject(object string) bool {
	re := regexp.MustCompile(`^` + regexp.QuoteMeta(c.prefix) + `\d{8}-\d{6}-[a-z0-9-]+\.[a-z0-9]{1,10}$`)
	return re.MatchString(object)
}

func videoIDExists(id string) bool {
	var n int
	db.QueryRow("SELECT COUNT(*) FROM videos WHERE video_id = ?", id).Scan(&n)
	return n > 0
}

// validateNewVideo checks title and video_id for a video about to be created.
// Returns the HTTP status and message on error.
func validateNewVideo(title, videoID string) (int, string) {
	if title == "" || len([]rune(title)) > 200 {
		return http.StatusBadRequest, "el título es obligatorio (máx 200 caracteres)"
	}
	if !videoIDRegex.MatchString(videoID) {
		return http.StatusBadRequest, "ID de vídeo no válido: solo minúsculas, números y guiones (máx 80)"
	}
	if videoIDExists(videoID) {
		return http.StatusConflict, fmt.Sprintf("ya existe un vídeo con el ID %q", videoID)
	}
	return 0, ""
}

// uploadOrigin is the browser origin the resumable session is bound to.
// It must be the admin's public origin (PUBLIC_BASE_URL, or the request
// host). A different Origin header would make the later PUTs fail by CORS,
// so it is rejected upfront.
func uploadOrigin(r *http.Request) (string, error) {
	want := originOf(baseURL(r))
	if got := r.Header.Get("Origin"); got != "" && !strings.EqualFold(strings.TrimRight(got, "/"), want) {
		return "", fmt.Errorf("el origen del navegador (%s) no coincide con el del servidor (%s); revisa PUBLIC_BASE_URL", got, want)
	}
	return want, nil
}

func originOf(base string) string {
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return strings.TrimRight(base, "/")
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
}

// ===== Handlers =====

const gcsDisabledMsg = "la subida a Google Cloud Storage no está configurada (falta GCS_BUCKET)"

// handleUploadStart: POST /api/upload/start {filename, size, content_type[, title, video_id]}
func handleUploadStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "método no permitido")
		return
	}
	if gcs == nil {
		writeJSONError(w, http.StatusServiceUnavailable, gcsDisabledMsg)
		return
	}
	var req struct {
		Filename    string `json:"filename"`
		Size        int64  `json:"size"`
		ContentType string `json:"content_type"`
		Title       string `json:"title"`
		VideoID     string `json:"video_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "JSON no válido")
		return
	}
	req.Filename = strings.TrimSpace(req.Filename)
	if req.Filename == "" {
		writeJSONError(w, http.StatusBadRequest, "falta el nombre del archivo")
		return
	}
	ct := strings.ToLower(strings.TrimSpace(req.ContentType))
	if ct == "" { // some browsers leave File.type empty for .mkv/.mov
		ct = videoTypeByExt(path.Ext(req.Filename))
	}
	if !strings.HasPrefix(ct, "video/") || strings.ContainsAny(ct, " ;,\r\n") {
		writeJSONError(w, http.StatusBadRequest, "solo se pueden subir vídeos (tipo video/*)")
		return
	}
	if req.Size <= 0 {
		writeJSONError(w, http.StatusBadRequest, "el archivo está vacío")
		return
	}
	if req.Size > maxUploadBytes {
		writeJSONError(w, http.StatusBadRequest, "el archivo es demasiado grande (máx 5 GB)")
		return
	}
	// Optional early check so a 3 GB upload doesn't end in "ID already exists".
	if req.VideoID != "" || req.Title != "" {
		if status, msg := validateNewVideo(strings.TrimSpace(req.Title), req.VideoID); status != 0 {
			writeJSONError(w, status, msg)
			return
		}
	}
	origin, err := uploadOrigin(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	object := uploadObjectName(gcs.prefix, req.Filename, ct, time.Now())
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	sessionURL, err := gcs.startResumable(ctx, object, ct, req.Size, origin)
	if err != nil {
		log.Printf("ERROR: no se pudo crear la sesión de subida para %s: %v", object, err)
		writeJSONError(w, http.StatusBadGateway, "no se pudo iniciar la subida en Google Cloud Storage: "+err.Error())
		return
	}
	log.Printf("Sesión de subida creada - objeto: gs://%s/%s, %d bytes, %s, origen %s", gcs.bucket, object, req.Size, ct, origin)
	writeJSON(w, http.StatusOK, map[string]string{
		"session_url": sessionURL,
		"object":      object,
		"public_url":  gcs.publicURL(object),
	})
}

// handleUploadComplete: POST /api/upload/complete {object, title, video_id}
// Verifies the object really exists in the bucket and registers the video.
func handleUploadComplete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "método no permitido")
		return
	}
	if gcs == nil {
		writeJSONError(w, http.StatusServiceUnavailable, gcsDisabledMsg)
		return
	}
	var req struct {
		Object  string `json:"object"`
		Title   string `json:"title"`
		VideoID string `json:"video_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "JSON no válido")
		return
	}
	req.Title = strings.TrimSpace(req.Title)
	if !gcs.validObject(req.Object) {
		writeJSONError(w, http.StatusBadRequest, "nombre de objeto no válido")
		return
	}
	if status, msg := validateNewVideo(req.Title, req.VideoID); status != 0 {
		writeJSONError(w, status, msg)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	obj, err := gcs.objectMetadata(ctx, req.Object)
	if errors.Is(err, errObjectNotFound) {
		writeJSONError(w, http.StatusNotFound, "el archivo no está en el bucket: la subida no terminó")
		return
	} else if err != nil {
		log.Printf("ERROR: no se pudo comprobar gs://%s/%s: %v", gcs.bucket, req.Object, err)
		writeJSONError(w, http.StatusBadGateway, "no se pudo comprobar el archivo en Google Cloud Storage: "+err.Error())
		return
	}

	video := Video{VideoID: req.VideoID, Title: req.Title, URL: gcs.publicURL(req.Object)}
	res, err := db.Exec("INSERT INTO videos (video_id, title, url, thumbnail) VALUES (?, ?, ?, '')", video.VideoID, video.Title, video.URL)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			writeJSONError(w, http.StatusConflict, fmt.Sprintf("ya existe un vídeo con el ID %q", video.VideoID))
			return
		}
		log.Printf("ERROR: no se pudo crear el vídeo: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "no se pudo crear el vídeo")
		return
	}
	video.ID, _ = res.LastInsertId()
	db.QueryRow("SELECT created_at, updated_at FROM videos WHERE id = ?", video.ID).Scan(&video.CreatedAt, &video.UpdatedAt)
	log.Printf("Vídeo subido y creado - video_id: %s, objeto: %s (%s bytes, %s)", video.VideoID, obj.Name, obj.Size, obj.ContentType)
	writeJSON(w, http.StatusCreated, video)
}
