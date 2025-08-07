#!/bin/bash

# VideoTracker Installation Script for Ubuntu/Debian
# This script installs and configures the VideoTracker service

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Configuration
SERVICE_USER="videotracker"
SERVICE_GROUP="videotracker"
INSTALL_DIR="/opt/videotracker"
SERVICE_FILE="videotracker.service"
BINARY_NAME="videotracker"

echo -e "${GREEN}VideoTracker Installation Script${NC}"
echo "=================================="

# Check if running as root
if [ "$EUID" -ne 0 ]; then 
    echo -e "${RED}Error: This script must be run as root (use sudo)${NC}"
    exit 1
fi

# Check if Ubuntu/Debian
if ! command -v systemctl &> /dev/null; then
    echo -e "${RED}Error: systemd not found. This script requires Ubuntu/Debian with systemd.${NC}"
    exit 1
fi

# Stop existing service if running
echo -e "${YELLOW}Stopping existing service (if any)...${NC}"
systemctl stop videotracker 2>/dev/null || true
systemctl disable videotracker 2>/dev/null || true

# Create service user and group
echo -e "${YELLOW}Creating service user and group...${NC}"
if ! id "$SERVICE_USER" &>/dev/null; then
    useradd --system --no-create-home --shell /bin/false "$SERVICE_USER"
    echo "Created user: $SERVICE_USER"
else
    echo "User $SERVICE_USER already exists"
fi

# Create installation directory
echo -e "${YELLOW}Creating installation directory...${NC}"
mkdir -p "$INSTALL_DIR"
mkdir -p "$INSTALL_DIR/static/images"
mkdir -p "$INSTALL_DIR/static/videos"
mkdir -p "$INSTALL_DIR/templates"

# Copy files
echo -e "${YELLOW}Installing VideoTracker binary and assets...${NC}"

# Copy binary
if [ -f "./videotracker-linux" ]; then
    cp "./videotracker-linux" "$INSTALL_DIR/$BINARY_NAME"
    chmod +x "$INSTALL_DIR/$BINARY_NAME"
    echo "✓ Binary installed"
else
    echo -e "${RED}Error: videotracker-linux binary not found in current directory${NC}"
    exit 1
fi

# Copy templates
if [ -d "./templates" ]; then
    cp -r ./templates/* "$INSTALL_DIR/templates/"
    echo "✓ Templates installed"
else
    echo -e "${RED}Error: templates directory not found${NC}"
    exit 1
fi

# Copy static assets
if [ -d "./static" ]; then
    cp -r ./static/* "$INSTALL_DIR/static/"
    echo "✓ Static assets installed"
else
    echo -e "${YELLOW}Warning: static directory not found, creating empty structure${NC}"
fi

# Set ownership
echo -e "${YELLOW}Setting file permissions...${NC}"
chown -R "$SERVICE_USER:$SERVICE_GROUP" "$INSTALL_DIR"
chmod 755 "$INSTALL_DIR"
chmod 755 "$INSTALL_DIR/$BINARY_NAME"

# Install systemd service
echo -e "${YELLOW}Installing systemd service...${NC}"
if [ -f "./$SERVICE_FILE" ]; then
    cp "./$SERVICE_FILE" "/etc/systemd/system/"
    systemctl daemon-reload
    echo "✓ Service installed"
else
    echo -e "${RED}Error: $SERVICE_FILE not found${NC}"
    exit 1
fi

# Enable and start service
echo -e "${YELLOW}Enabling and starting VideoTracker service...${NC}"
systemctl enable videotracker
systemctl start videotracker

# Check service status
echo -e "${YELLOW}Checking service status...${NC}"
sleep 2
if systemctl is-active --quiet videotracker; then
    echo -e "${GREEN}✓ VideoTracker service is running successfully!${NC}"
else
    echo -e "${RED}✗ Service failed to start. Check logs with: sudo journalctl -u videotracker${NC}"
    exit 1
fi

# Show service information
echo ""
echo -e "${GREEN}Installation Complete!${NC}"
echo "====================="
echo "• Service Status: $(systemctl is-active videotracker)"
echo "• Installation Directory: $INSTALL_DIR"
echo "• Log Files: journalctl -u videotracker -f"
echo "• Configuration: Edit $INSTALL_DIR files and restart service"
echo ""
echo -e "${GREEN}Access URLs:${NC}"
echo "• Admin Dashboard: http://localhost:8080/admin"
echo "• Watch Page: http://localhost:8080/watch?user_id=test&video_id=sample"
echo ""
echo -e "${GREEN}Management Commands:${NC}"
echo "• Start:   sudo systemctl start videotracker"
echo "• Stop:    sudo systemctl stop videotracker" 
echo "• Restart: sudo systemctl restart videotracker"
echo "• Status:  sudo systemctl status videotracker"
echo "• Logs:    sudo journalctl -u videotracker -f"
echo ""
echo -e "${YELLOW}Note: Make sure port 8080 is open in your firewall if needed.${NC}"