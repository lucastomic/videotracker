package main

import (
	"database/sql"
	"encoding/json"
	"html/template"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Session struct {
	ID               int64           `json:"id"`
	UserID           string          `json:"user_id"`
	VideoID          string          `json:"video_id"`
	IPAddress        string          `json:"ip_address"`
	EnteredAt        time.Time       `json:"entered_at"`
	LeftAt           *time.Time      `json:"left_at"`
	TotalTimeWatched int             `json:"total_time_watched"`
	PlayedSeconds    json.RawMessage `json:"played_seconds"`
}

type Video struct {
	ID        int64     `json:"id"`
	VideoID   string    `json:"video_id"`
	Title     string    `json:"title"`
	URL       string    `json:"url"`
	Thumbnail string    `json:"thumbnail"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

var db *sql.DB
var templates *template.Template

// loggingMiddleware logs all HTTP requests
func loggingMiddleware(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		log.Printf("[%s] %s %s - User-Agent: %s", r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("User-Agent"))
		handler(w, r)
		log.Printf("[%s] %s completed in %v", r.Method, r.URL.Path, time.Since(start))
	}
}

// getClientIP extracts the client IP address from the request
func getClientIP(r *http.Request) string {
	// Check X-Forwarded-For header first (for proxies)
	forwarded := r.Header.Get("X-Forwarded-For")
	if forwarded != "" {
		// Behind the GCP external load balancer (Gateway), the LB appends
		// "<client-ip>, <lb-ip>" to whatever the client sent. The first values
		// are client-controlled and spoofable (which would bypass the login/
		// password rate limits), so use the second-to-last one, which the LB
		// itself wrote. With a single value, that is the one to use.
		parts := strings.Split(forwarded, ",")
		if len(parts) >= 2 {
			return strings.TrimSpace(parts[len(parts)-2])
		}
		return strings.TrimSpace(parts[0])
	}

	// Check X-Real-IP header
	realIP := r.Header.Get("X-Real-IP")
	if realIP != "" {
		return realIP
	}

	// Fall back to RemoteAddr
	// Remove port if present (also handles "[::1]:1234")
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func main() {
	var err error

	// Get database path from environment or use default
	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "./video_tracker.db"
	}

	// Initialize database
	db, err = sql.Open("sqlite3", dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	// Create tables
	createTables()

	configureGCS()

	// Thumbnails for share links live next to the database (/app/data/og).
	ogDir = os.Getenv("OG_DIR")
	if ogDir == "" {
		ogDir = filepath.Join(filepath.Dir(dbPath), "og")
	}

	configureAuth()

	// Parse templates
	templates = template.Must(template.ParseGlob("templates/*.html"))

	log.Println("=== Video Tracker Server Starting ===")
	log.Println("Server running on: http://localhost:8080")
	log.Println("Admin dashboard: http://localhost:8080/admin")
	log.Println("Watch page example: http://localhost:8080/watch?user_id=test123&video_id=boatexperience")
	log.Fatal(http.ListenAndServe(":8080", newRouter()))
}

// newRouter wires every route. Admin pages/APIs go through requireAdmin.
func newRouter() *http.ServeMux {
	mux := http.NewServeMux()
	page := func(h http.HandlerFunc) http.HandlerFunc { return loggingMiddleware(requireAdmin(h, false)) }
	api := func(h http.HandlerFunc) http.HandlerFunc { return loggingMiddleware(requireAdmin(h, true)) }

	// Public
	mux.HandleFunc("/", loggingMiddleware(handleHome))
	mux.HandleFunc("/watch", loggingMiddleware(handleWatch))
	mux.HandleFunc("/track", loggingMiddleware(handleTrack))
	mux.HandleFunc("/session/end", loggingMiddleware(handleSessionEnd))
	mux.HandleFunc("/v/", loggingMiddleware(handleShare))
	mux.HandleFunc("/og/", loggingMiddleware(handleOGImage))
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))

	// Admin login
	mux.HandleFunc("/admin/login", loggingMiddleware(handleLogin))
	mux.HandleFunc("/admin/logout", loggingMiddleware(handleLogout))

	// Admin (protected)
	mux.HandleFunc("/admin", page(handleAdmin))
	mux.HandleFunc("/admin/", page(http.NotFound))
	mux.HandleFunc("/api/videos", api(handleVideos))
	mux.HandleFunc("/api/videos/", api(handleVideoByID))
	mux.HandleFunc("/api/admin/data", api(handleAdminData))
	mux.HandleFunc("/api/admin/shares", api(handleAdminShares))
	mux.HandleFunc("/api/admin/shares/", api(handleAdminShareByID))
	mux.HandleFunc("/api/admin/", api(func(w http.ResponseWriter, r *http.Request) {
		writeJSONError(w, http.StatusNotFound, "no encontrado")
	}))
	mux.HandleFunc("/api/upload/start", api(handleUploadStart))
	mux.HandleFunc("/api/upload/complete", api(handleUploadComplete))
	return mux
}

func createTables() {
	query := `
	CREATE TABLE IF NOT EXISTS sessions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id TEXT NOT NULL,
		video_id TEXT NOT NULL,
		ip_address TEXT DEFAULT '',
		entered_at DATETIME NOT NULL,
		left_at DATETIME,
		total_time_watched INTEGER DEFAULT 0,
		played_seconds TEXT DEFAULT '[]'
	);

	CREATE TABLE IF NOT EXISTS videos (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		video_id TEXT UNIQUE NOT NULL,
		title TEXT NOT NULL,
		url TEXT NOT NULL,
		thumbnail TEXT DEFAULT '',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	`
	if _, err := db.Exec(query); err != nil {
		log.Fatal(err)
	}

	// Columns added after the first release (idempotent migrations).
	ensureColumn("sessions", "ip_address", "TEXT DEFAULT ''")
	ensureColumn("videos", "thumbnail", "TEXT DEFAULT ''")

	_, err := db.Exec(`
	CREATE INDEX IF NOT EXISTS idx_user_id ON sessions(user_id);
	CREATE INDEX IF NOT EXISTS idx_video_id ON sessions(video_id);
	CREATE INDEX IF NOT EXISTS idx_ip_address ON sessions(ip_address);
	`)
	if err != nil {
		log.Fatal(err)
	}

	createShareTables()
	log.Println("Database tables initialized")
}

func handleHome(w http.ResponseWriter, r *http.Request) {
	err := templates.ExecuteTemplate(w, "home_standalone.html", nil)
	if err != nil {
		log.Printf("ERROR: Failed to render home template: %v", err)
		http.Error(w, "Failed to render page", http.StatusInternalServerError)
	}
}

func handleWatch(w http.ResponseWriter, r *http.Request) {
	userID := r.URL.Query().Get("user_id")
	videoID := r.URL.Query().Get("video_id")

	if videoID == "" {
		log.Printf("ERROR: Missing video_id parameter")
		http.Error(w, "Missing video_id", http.StatusBadRequest)
		return
	}

	// Default user_id to anonymous + IP if not provided
	if userID == "" {
		userID = "anonymous-" + getClientIP(r)
	}

	// Get client IP
	clientIP := getClientIP(r)
	log.Printf("Creating new session - User: %s, Video: %s, IP: %s", userID, videoID, clientIP)

	// Lookup video URL from database
	var video Video
	err := db.QueryRow(
		"SELECT id, video_id, title, url, COALESCE(thumbnail, '') FROM videos WHERE video_id = ?",
		videoID,
	).Scan(&video.ID, &video.VideoID, &video.Title, &video.URL, &video.Thumbnail)

	if err == sql.ErrNoRows {
		log.Printf("ERROR: Video not found - video_id: %s", videoID)
		http.Error(w, "Video not found", http.StatusNotFound)
		return
	} else if err != nil {
		log.Printf("ERROR: Failed to lookup video: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Create new session with IP address
	result, err := db.Exec(
		"INSERT INTO sessions (user_id, video_id, ip_address, entered_at) VALUES (?, ?, ?, ?)",
		userID, videoID, clientIP, time.Now(),
	)
	if err != nil {
		log.Printf("ERROR: Failed to create session: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	sessionID, _ := result.LastInsertId()
	log.Printf("Session created successfully - Session ID: %d", sessionID)

	// Get the full page URL for Open Graph
	base := baseURL(r)
	pageURL := base + r.URL.String()

	// Use video thumbnail if available, otherwise default logo
	thumbnail := video.Thumbnail
	if thumbnail == "" {
		thumbnail = base + "/static/images/logo.png"
	} else if !strings.HasPrefix(thumbnail, "http") {
		thumbnail = base + thumbnail
	}

	data := watchData{
		UserID:     userID,
		VideoID:    videoID,
		VideoURL:   video.URL,
		VideoTitle: video.Title,
		SessionID:  sessionID,
		PageURL:    pageURL,
		VideoThumb: thumbnail,
	}

	err = templates.ExecuteTemplate(w, "watch_standalone.html", data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// watchData feeds watch_standalone.html (legacy /watch and share links /v/).
type watchData struct {
	UserID     string
	VideoID    string
	VideoURL   string
	VideoTitle string
	SessionID  int64
	PageURL    string
	VideoThumb string
	OG         *OGMeta // set for share links: summary_large_image tags
}

func handleTrack(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	sessionID := r.FormValue("session_id")
	playedSecondsStr := r.FormValue("played_seconds")

	log.Printf("Tracking update - Session: %s, New seconds: %s", sessionID, playedSecondsStr)

	// Parse played seconds
	var newSeconds []int
	if err := json.Unmarshal([]byte(playedSecondsStr), &newSeconds); err != nil {
		log.Printf("ERROR: Invalid played_seconds format: %v", err)
		http.Error(w, "Invalid played_seconds format", http.StatusBadRequest)
		return
	}

	// Get existing played seconds
	var existingSecondsJSON string
	err := db.QueryRow("SELECT played_seconds FROM sessions WHERE id = ?", sessionID).Scan(&existingSecondsJSON)
	if err != nil {
		log.Printf("ERROR: Session not found: %v", err)
		http.Error(w, "Session not found", http.StatusNotFound)
		return
	}

	var existingSeconds []int
	json.Unmarshal([]byte(existingSecondsJSON), &existingSeconds)

	// Merge and deduplicate
	secondsMap := make(map[int]bool)
	for _, s := range existingSeconds {
		secondsMap[s] = true
	}
	for _, s := range newSeconds {
		secondsMap[s] = true
	}

	// Convert back to sorted slice
	var allSeconds []int
	for s := range secondsMap {
		allSeconds = append(allSeconds, s)
	}

	// Update database
	allSecondsJSON, _ := json.Marshal(allSeconds)
	totalTime := len(allSeconds)

	_, err = db.Exec(
		"UPDATE sessions SET played_seconds = ?, total_time_watched = ? WHERE id = ?",
		string(allSecondsJSON), totalTime, sessionID,
	)
	if err != nil {
		log.Printf("ERROR: Failed to update session: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	log.Printf("Session %s updated - Total watched: %d seconds, New total: %d unique seconds", sessionID, totalTime, len(allSeconds))

	// Return empty response for HTMX
	w.WriteHeader(http.StatusOK)
}

func handleSessionEnd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	sessionID := r.FormValue("session_id")
	log.Printf("Ending session: %s", sessionID)

	_, err := db.Exec(
		"UPDATE sessions SET left_at = ? WHERE id = ?",
		time.Now(), sessionID,
	)
	if err != nil {
		log.Printf("ERROR: Failed to end session: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	log.Printf("Session %s ended successfully", sessionID)
	w.WriteHeader(http.StatusOK)
}

func handleAdmin(w http.ResponseWriter, r *http.Request) {
	log.Println("Loading admin dashboard")

	// Get sessions
	rows, err := db.Query(`
		SELECT id, user_id, video_id, 
		       CASE WHEN EXISTS(SELECT 1 FROM pragma_table_info('sessions') WHERE name='ip_address') 
		            THEN ip_address ELSE '' END as ip_address,
		       entered_at, left_at, total_time_watched, played_seconds
		FROM sessions
		ORDER BY entered_at DESC
		LIMIT 100
	`)
	if err != nil {
		// Fallback query without ip_address
		rows, err = db.Query(`
			SELECT id, user_id, video_id, entered_at, left_at, total_time_watched, played_seconds
			FROM sessions
			ORDER BY entered_at DESC
			LIMIT 100
		`)
		if err != nil {
			log.Printf("ERROR: Failed to query sessions: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	defer rows.Close()

	var sessions []Session
	hasIPColumn := true

	for rows.Next() {
		var s Session
		var leftAt sql.NullTime
		var playedSecondsStr string

		var err error
		if hasIPColumn {
			err = rows.Scan(&s.ID, &s.UserID, &s.VideoID, &s.IPAddress, &s.EnteredAt, &leftAt, &s.TotalTimeWatched, &playedSecondsStr)
			if err != nil {
				// Try without IP column
				hasIPColumn = false
				err = rows.Scan(&s.ID, &s.UserID, &s.VideoID, &s.EnteredAt, &leftAt, &s.TotalTimeWatched, &playedSecondsStr)
			}
		} else {
			err = rows.Scan(&s.ID, &s.UserID, &s.VideoID, &s.EnteredAt, &leftAt, &s.TotalTimeWatched, &playedSecondsStr)
		}

		if err != nil {
			log.Printf("WARNING: Failed to scan row: %v", err)
			continue
		}

		if leftAt.Valid {
			s.LeftAt = &leftAt.Time
		}
		s.PlayedSeconds = json.RawMessage(playedSecondsStr)

		sessions = append(sessions, s)
	}

	// Get videos
	videoRows, err := db.Query("SELECT id, video_id, title, url, created_at, updated_at FROM videos ORDER BY created_at DESC")
	if err != nil {
		log.Printf("ERROR: Failed to query videos: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer videoRows.Close()

	var videos []Video
	for videoRows.Next() {
		var v Video
		err := videoRows.Scan(&v.ID, &v.VideoID, &v.Title, &v.URL, &v.CreatedAt, &v.UpdatedAt)
		if err != nil {
			log.Printf("WARNING: Failed to scan video row: %v", err)
			continue
		}
		videos = append(videos, v)
	}

	log.Printf("Loaded %d sessions and %d videos for admin dashboard", len(sessions), len(videos))

	data := struct {
		Sessions   []Session
		Videos     []Video
		GCSEnabled bool
		GCSBucket  string
	}{
		Sessions:   sessions,
		Videos:     videos,
		GCSEnabled: gcs != nil,
	}
	if gcs != nil {
		data.GCSBucket = gcs.bucket
	}

	err = templates.ExecuteTemplate(w, "admin_standalone.html", data)
	if err != nil {
		log.Printf("ERROR: Failed to render admin template: %v", err)
		http.Error(w, "Failed to render page", http.StatusInternalServerError)
		return
	}
}

// handleVideos handles CRUD operations for videos
func handleVideos(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		getVideos(w, r)
	case http.MethodPost:
		createVideo(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleVideoByID handles operations on specific video by ID
func handleVideoByID(w http.ResponseWriter, r *http.Request) {
	// Extract video_id from URL path
	path := strings.TrimPrefix(r.URL.Path, "/api/videos/")
	if path == "" {
		http.Error(w, "Video ID required", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		getVideoByID(w, r, path)
	case http.MethodPut:
		updateVideo(w, r, path)
	case http.MethodDelete:
		deleteVideo(w, r, path)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// getVideos returns all videos as JSON
func getVideos(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query("SELECT id, video_id, title, url, COALESCE(thumbnail, ''), created_at, updated_at FROM videos ORDER BY created_at DESC")
	if err != nil {
		log.Printf("ERROR: Failed to query videos: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var videos []Video
	for rows.Next() {
		var v Video
		err := rows.Scan(&v.ID, &v.VideoID, &v.Title, &v.URL, &v.Thumbnail, &v.CreatedAt, &v.UpdatedAt)
		if err != nil {
			log.Printf("WARNING: Failed to scan video row: %v", err)
			continue
		}
		videos = append(videos, v)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(videos)
}

// createVideo creates a new video record
func createVideo(w http.ResponseWriter, r *http.Request) {
	var video Video
	if err := json.NewDecoder(r.Body).Decode(&video); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	if video.VideoID == "" || video.Title == "" || video.URL == "" {
		http.Error(w, "video_id, title, and url are required", http.StatusBadRequest)
		return
	}

	result, err := db.Exec(
		"INSERT INTO videos (video_id, title, url, thumbnail) VALUES (?, ?, ?, ?)",
		video.VideoID, video.Title, video.URL, video.Thumbnail,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			http.Error(w, "Video ID already exists", http.StatusConflict)
		} else {
			log.Printf("ERROR: Failed to create video: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	id, _ := result.LastInsertId()
	video.ID = id

	// Get the created video with timestamps
	err = db.QueryRow("SELECT created_at, updated_at FROM videos WHERE id = ?", id).Scan(&video.CreatedAt, &video.UpdatedAt)
	if err != nil {
		log.Printf("WARNING: Failed to get timestamps: %v", err)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(video)
}

// getVideoByID returns a specific video by video_id
func getVideoByID(w http.ResponseWriter, r *http.Request, videoID string) {
	var video Video
	err := db.QueryRow(
		"SELECT id, video_id, title, url, COALESCE(thumbnail, ''), created_at, updated_at FROM videos WHERE video_id = ?",
		videoID,
	).Scan(&video.ID, &video.VideoID, &video.Title, &video.URL, &video.Thumbnail, &video.CreatedAt, &video.UpdatedAt)

	if err == sql.ErrNoRows {
		http.Error(w, "Video not found", http.StatusNotFound)
		return
	} else if err != nil {
		log.Printf("ERROR: Failed to get video: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(video)
}

// updateVideo updates an existing video
func updateVideo(w http.ResponseWriter, r *http.Request, videoID string) {
	var video Video
	if err := json.NewDecoder(r.Body).Decode(&video); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	if video.Title == "" || video.URL == "" {
		http.Error(w, "title and url are required", http.StatusBadRequest)
		return
	}

	result, err := db.Exec(
		"UPDATE videos SET title = ?, url = ?, thumbnail = ?, updated_at = CURRENT_TIMESTAMP WHERE video_id = ?",
		video.Title, video.URL, video.Thumbnail, videoID,
	)
	if err != nil {
		log.Printf("ERROR: Failed to update video: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		http.Error(w, "Video not found", http.StatusNotFound)
		return
	}

	// Get updated video
	getVideoByID(w, r, videoID)
}

// deleteVideo deletes a video
func deleteVideo(w http.ResponseWriter, r *http.Request, videoID string) {
	result, err := db.Exec("DELETE FROM videos WHERE video_id = ?", videoID)
	if err != nil {
		log.Printf("ERROR: Failed to delete video: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		http.Error(w, "Video not found", http.StatusNotFound)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleAdminData returns admin dashboard data as JSON for AJAX polling
func handleAdminData(w http.ResponseWriter, r *http.Request) {
	// Get sessions
	rows, err := db.Query(`
		SELECT id, user_id, video_id, COALESCE(ip_address, '') as ip_address,
		       entered_at, left_at, total_time_watched, played_seconds
		FROM sessions
		ORDER BY entered_at DESC
		LIMIT 100
	`)
	if err != nil {
		log.Printf("ERROR: Failed to query sessions: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var sessions []Session
	for rows.Next() {
		var s Session
		var leftAt sql.NullTime
		var playedSecondsStr string

		err := rows.Scan(&s.ID, &s.UserID, &s.VideoID, &s.IPAddress, &s.EnteredAt, &leftAt, &s.TotalTimeWatched, &playedSecondsStr)
		if err != nil {
			log.Printf("WARNING: Failed to scan row: %v", err)
			continue
		}

		if leftAt.Valid {
			s.LeftAt = &leftAt.Time
		}
		s.PlayedSeconds = json.RawMessage(playedSecondsStr)
		sessions = append(sessions, s)
	}

	// Get videos
	videoRows, err := db.Query("SELECT id, video_id, title, url, COALESCE(thumbnail, ''), created_at, updated_at FROM videos ORDER BY created_at DESC")
	if err != nil {
		log.Printf("ERROR: Failed to query videos: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer videoRows.Close()

	var videos []Video
	for videoRows.Next() {
		var v Video
		err := videoRows.Scan(&v.ID, &v.VideoID, &v.Title, &v.URL, &v.Thumbnail, &v.CreatedAt, &v.UpdatedAt)
		if err != nil {
			log.Printf("WARNING: Failed to scan video row: %v", err)
			continue
		}
		videos = append(videos, v)
	}

	resp := struct {
		Sessions   []Session `json:"sessions"`
		Videos     []Video   `json:"videos"`
		GCSEnabled bool      `json:"gcs_enabled"`
	}{
		Sessions:   sessions,
		Videos:     videos,
		GCSEnabled: gcs != nil,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
