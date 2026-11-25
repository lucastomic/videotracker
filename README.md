# Video Tracker

A minimal web application to track video watch time using Go, HTMX, and HTML templates.

## Features

- Track user video watching sessions with entry/exit timestamps
- Record which seconds of the video were watched (with deduplication)
- Automatic tracking updates every 5 seconds during playback
- Admin dashboard to view all sessions and analytics
- No JavaScript build tools - uses vanilla JS and HTMX from CDN
- SQLite database for simple deployment

## Configuración (variables de entorno)

| Variable | Obligatoria | Descripción |
|---|---|---|
| `ADMIN_PASSWORD` | **Sí** | Contraseña del panel `/admin`. Si falta, la app arranca pero todo lo de admin responde `503 ADMIN_PASSWORD no configurada` (y lo avisa en el log). Las páginas públicas siguen funcionando. |
| `SESSION_SECRET` | No | Clave para firmar (HMAC) las cookies de sesión de admin y de enlaces con contraseña. Si no se define se deriva de `ADMIN_PASSWORD` (cambiar la contraseña cierra todas las sesiones). |
| `DB_PATH` | No | Ruta del SQLite (por defecto `./video_tracker.db`; en GKE `/app/data/video_tracker.db`). |
| `OG_DIR` | No | Carpeta de las miniaturas de WhatsApp. Por defecto `og/` junto a la base de datos (`/app/data/og`). |
| `PUBLIC_BASE_URL` | No | URL pública, p. ej. `https://demos.solnow.io`, para las etiquetas Open Graph. Si no se define se usa `X-Forwarded-Proto` + `Host`. |
| `GCS_BUCKET` | No | Bucket de Google Cloud Storage para subir vídeos desde el admin (p. ej. `solnow-demos`). Si falta, la zona de subida aparece desactivada y `/api/upload/*` responde `503`. |
| `GCS_PREFIX` | No | Carpeta dentro del bucket. Por defecto `videos/`. |

> Las variables antiguas de S3 (`S3_BUCKET`, `AWS_*`) ya no se usan. Los vídeos dados de alta con URL de S3 siguen funcionando: solo son URLs.

### Acceso al admin

- `/admin/login` (formulario) y `/admin/logout`. La sesión es una cookie `HttpOnly; Secure; SameSite=Lax` firmada, válida 30 días.
- Máximo 10 intentos fallidos por IP cada 15 minutos (IP = primer valor de `X-Forwarded-For`).
- Protegido: `/admin`, `/api/videos`, `/api/videos/*`, `/api/admin/*`, `/api/upload/start`, `/api/upload/complete`. Sin sesión, las APIs devuelven `401` JSON y las páginas redirigen al login.
- Público: `/`, `/watch`, `/track`, `/session/end`, `/static/*`, `/v/{token}`, `/og/{token}.jpg`. Los enlaces antiguos `/watch?video_id=..&user_id=..` siguen funcionando igual.

> Nota: las cookies llevan `Secure`, así que el login necesita HTTPS (o `http://localhost`, que los navegadores tratan como seguro).

## Subir vídeos a Google Cloud Storage

En el admin, arriba de **Video Management**, arrastra uno o varios vídeos a la zona (o haz clic para elegirlos). Para cada archivo revisa el **Título** (por defecto, el nombre del archivo) y el **ID del vídeo** (por defecto, el título en slug; solo `a-z`, `0-9` y `-`, único) y pulsa **Subir**. Los archivos se suben uno detrás de otro, con barra de progreso, MB, velocidad y tiempo restante; se puede cancelar. Al terminar, el vídeo queda dado de alta con la URL pública `https://storage.googleapis.com/<bucket>/<objeto>` y aparece **Crear enlace para compartir** con ese vídeo ya elegido.

Cómo funciona (los 1–3 GB **no pasan por el servidor**):

1. `POST /api/upload/start` `{filename, size, content_type, title, video_id}`: valida (solo `video/*`, máx. 5 GB, ID válido y libre), elige el nombre `videos/<AAAAMMDD-HHMMSS>-<slug>.<ext>` y abre una **sesión de subida reanudable** de GCS con el token de la cuenta de servicio (Workload Identity; sin claves ni `signBlob`). La sesión se ata al origen del navegador (`PUBLIC_BASE_URL`, o `X-Forwarded-Proto` + `Host`). Se usa `ifGenerationMatch=0`, así que nunca sobrescribe un objeto existente.
2. El navegador hace `PUT` al `session_url` en trozos de 16 MiB con `Content-Range` (lee el archivo con `File.slice`, sin cargarlo entero en memoria). `308` = sigue (la cabecera `Range` dice hasta dónde llegó); `200/201` = terminado. Ante error de red, `5xx`, `408` o `429`, espera exponencial (1 s → 60 s, 8 reintentos) y pregunta el estado con un `PUT` vacío `Content-Range: bytes */<tamaño>`. Si se agotan los reintentos, el botón **Reanudar** continúa la misma sesión.
3. `POST /api/upload/complete` `{object, title, video_id}`: comprueba en GCS que el objeto existe (metadatos por la JSON API) y crea el vídeo.

### Permisos y configuración del bucket

- La cuenta de servicio de Google del pod (Workload Identity) necesita **`roles/storage.objectAdmin`** en el bucket:

  ```bash
  gcloud storage buckets add-iam-policy-binding gs://solnow-demos \
    --member="serviceAccount:<GSA>@<PROYECTO>.iam.gserviceaccount.com" --role="roles/storage.objectAdmin"
  ```

- Los objetos deben poder leerse públicamente (los reproduce el navegador del cliente), p. ej. `allUsers` con `roles/storage.objectViewer` en el bucket.
- **CORS** del bucket para el origen del admin (sin esto el navegador no puede subir):

  ```json
  [
    {
      "origin": ["https://demos.solnow.io"],
      "method": ["PUT", "POST", "GET"],
      "responseHeader": ["Content-Type", "Content-Range", "Range", "x-goog-resumable"],
      "maxAgeSeconds": 3600
    }
  ]
  ```

  ```bash
  gcloud storage buckets update gs://solnow-demos --cors-file=cors.json
  ```

  `Range` tiene que estar en `responseHeader` (cabecera expuesta): el navegador la lee para saber cuánto ha recibido GCS. Para probar en local añade también `http://localhost:8080` al `origin` y define `PUBLIC_BASE_URL=http://localhost:8080`.
- En local la app usa las Application Default Credentials (`gcloud auth application-default login`).

## Enlaces para compartir (`/v/{token}`)

En el admin, sección **Enlaces para compartir**:

1. Elige el **vídeo** y escribe el **destinatario** (etiqueta con la que aparecerán sus visitas en el seguimiento, hace de `user_id`).
2. Opcional: pega la **web de la empresa** y pulsa **Analizar web**. El servidor descarga la página y propone nombre, logo y color de marca. Puedes corregirlos; la vista previa de la miniatura se actualiza al momento.
3. Opcional: **contraseña** y **caducidad** (7/30/90 días o una fecha concreta).
4. **Crear enlace** → copia la URL `https://demos.solnow.io/v/XXXXXXXXXXXXXXXX` y envíala por WhatsApp (y la contraseña, si la tiene, por separado).

En la tabla verás estado (activo / caducado / revocado), visitas, tiempo visto, última visita, la miniatura, y botones para copiar, editar la marca (regenera la miniatura) o revocar.

Detalles:

- Las etiquetas Open Graph (`og:title`, `og:description`, `og:image` 1200x630, `twitter:card=summary_large_image`...) salen siempre, también en la pantalla de contraseña, para que WhatsApp pinte la vista previa. La URL del vídeo no aparece en el HTML hasta acertar la contraseña.
- Enlace caducado o revocado → página "Este enlace ha caducado" (HTTP 410) con OG genéricas de SolNow.
- Las visitas de robots de vista previa (WhatsApp, Facebook, Telegram...) no cuentan como sesión.
- La miniatura (`/og/{token}.jpg`, JPEG < 300 KB, cacheable 1 día) se genera al crear/editar y se guarda en `OG_DIR`.
- La descarga de webs/logos solo la hace el admin y está protegida contra SSRF: solo http/https, se rechazan IPs privadas/loopback/link-local/metadata (también tras redirecciones, máx. 5), timeout 8 s y máximo 3 MB.

Ejemplo por API (con la cookie de admin):

```bash
curl -b 'vt_admin=...' -H 'Content-Type: application/json' \
  -d '{"video_id":"demo","recipient":"marina-ibiza-juan","company_url":"https://www.marinaibiza.com","password":"opcional","expires_in_days":30}' \
  https://demos.solnow.io/api/admin/shares
```

## Tests

```bash
go test ./...
```

## Project Structure

```
videoTracker/
├── main.go                 # Server, routes, video/session handlers
├── auth.go                 # Admin login, signed cookies, rate limiting
├── shares.go               # Share links (/v/{token}), OG images, admin API
├── fetchmeta.go            # SSRF-safe fetch + company name/logo/color extraction
├── ogimage.go              # 1200x630 WhatsApp thumbnail composition
├── gcsupload.go            # Direct browser -> GCS resumable uploads
├── templates/              # HTML templates
├── static/images/logo.png  # SolNow logo
└── go.mod                  # Go module file
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
