# Build stage
FROM --platform=linux/amd64 golang:1.24-alpine AS builder

# Install build dependencies
RUN apk add --no-cache gcc musl-dev

# Set working directory
WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY main.go .

# Build the application
RUN CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -ldflags="-w -s" -o videotracker main.go

# Runtime stage
FROM --platform=linux/amd64 alpine:3.19

# Install ca-certificates for HTTPS requests
RUN apk --no-cache add ca-certificates

# Create non-root user
RUN addgroup -g 1001 -S videotracker && \
    adduser -u 1001 -S videotracker -G videotracker

# Set working directory
WORKDIR /app

# Copy binary from builder
COPY --from=builder /app/videotracker .

# Copy templates and static files
COPY templates/ ./templates/
COPY static/ ./static/

# Create database directory and set ownership
RUN mkdir -p /app/data && \
    chown -R videotracker:videotracker /app && \
    chmod -R 755 /app

# Switch to non-root user
USER videotracker

# Create empty database file to ensure proper permissions
RUN touch video_tracker.db && chmod 644 video_tracker.db

# Expose port
EXPOSE 8080

# Set environment variables
ENV GIN_MODE=release

# Health check
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget --no-verbose --tries=1 --spider http://localhost:8080/ || exit 1

# Run the application
CMD ["./videotracker"]