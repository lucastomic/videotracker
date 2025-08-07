# Docker Deployment Guide

This guide explains how to run Video Tracker using Docker.

## Quick Start

### Option 1: Using Docker Compose (Recommended)

```bash
# Start the application
docker-compose up -d

# View logs
docker-compose logs -f

# Stop the application
docker-compose down
```

### Option 2: Using Docker directly

```bash
# Build the image
docker build -t videotracker .

# Run the container
docker run -d --name videotracker -p 8080:8080 \
  -v $(pwd)/data:/app/data \
  videotracker

# View logs
docker logs -f videotracker

# Stop the container
docker stop videotracker
docker rm videotracker
```

## Accessing the Application

Once running, the application will be available at:

- **Home**: http://localhost:8080/
- **Admin Dashboard**: http://localhost:8080/admin
- **Video Watch**: http://localhost:8080/watch?user_id=USERNAME&video_id=VIDEOID

## Data Persistence

The SQLite database is stored at `/app/video_tracker.db` inside the container.

With docker-compose, it's automatically mounted to `./data/video_tracker.db` on your host.

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| PORT | 8080 | Port the application listens on |

## Custom Configuration

### Custom Static Files
Mount your own static files:
```bash
docker run -d --name videotracker -p 8080:8080 \
  -v $(pwd)/custom-static:/app/static \
  videotracker
```

### Custom Templates
Mount your own templates:
```bash
docker run -d --name videotracker -p 8080:8080 \
  -v $(pwd)/custom-templates:/app/templates \
  videotracker
```

## Health Check

The container includes a health check that verifies the application is responding:

```bash
# Check container health
docker ps

# Manual health check
curl http://localhost:8080/
```

## Troubleshooting

### Check logs
```bash
docker logs videotracker
```

### Access container shell
```bash
docker exec -it videotracker /bin/sh
```

### Reset database
```bash
docker-compose down
sudo rm -f data/video_tracker.db
docker-compose up -d
```

## Security Considerations

- The container runs as a non-root user (`videotracker:videotracker`)
- Database files have proper permissions
- Only port 8080 is exposed
- No sensitive information is logged

## Building from Source

```bash
# Clone repository
git clone <repository-url>
cd videoTracker

# Build image
docker build -t videotracker .

# Run
docker run -d --name videotracker -p 8080:8080 videotracker
```