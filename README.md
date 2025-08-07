# Video Tracker

A minimal web application to track video watch time using Go, HTMX, and HTML templates.

## Features

- Track user video watching sessions with entry/exit timestamps
- Record which seconds of the video were watched (with deduplication)
- Automatic tracking updates every 5 seconds during playback
- Admin dashboard to view all sessions and analytics
- No JavaScript build tools - uses vanilla JS and HTMX from CDN
- SQLite database for simple deployment

## Project Structure

```
videoTracker/
├── main.go                 # Main server with routes and handlers
├── templates/
│   ├── base.html          # Base template with common styles
│   ├── watch.html         # Video player page with tracking
│   └── admin.html         # Admin analytics dashboard
├── static/
│   └── videos/            # Place your video files here
├── video_tracker.db       # SQLite database (created automatically)
└── go.mod                 # Go module file
```

## Getting Started

1. Place your video files in `static/videos/` directory:
   - Name them as `{video_id}.mp4` (e.g., `sample.mp4`, `demo.mp4`)

2. Run the application:
   ```bash
   go run main.go
   ```

3. Open your browser and visit:
   - Watch page: `http://localhost:8080/watch?user_id=user123&video_id=sample`
   - Admin dashboard: `http://localhost:8080/admin`

## How It Works

1. When a user visits `/watch?user_id=X&video_id=Y`:
   - A new session is created with `entered_at` timestamp
   - The video player loads the corresponding video file

2. During playback:
   - JavaScript tracks which seconds have been watched
   - Every 5 seconds, the watched seconds are sent to the server via HTMX
   - The server deduplicates and stores the watched seconds

3. When the user leaves:
   - The `beforeunload` event triggers a final update
   - The session is marked with `left_at` timestamp

4. The admin dashboard shows:
   - All sessions with user/video IDs
   - Entry and exit timestamps
   - Total seconds watched
   - Detailed list of watched seconds

## Database Schema

```sql
CREATE TABLE sessions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id TEXT NOT NULL,
    video_id TEXT NOT NULL,
    entered_at DATETIME NOT NULL,
    left_at DATETIME,
    total_time_watched INTEGER DEFAULT 0,
    played_seconds TEXT DEFAULT '[]'  -- JSON array of watched seconds
);
```

## Technologies Used

- **Backend**: Go standard library
- **Database**: SQLite with go-sqlite3 driver
- **Frontend**: HTML5 video element, vanilla JavaScript
- **AJAX**: HTMX for seamless updates
- **Templates**: Go's html/template package
