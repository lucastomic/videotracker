# VideoTracker - Ubuntu Deployment Guide

This guide explains how to deploy VideoTracker on Ubuntu/Debian systems.

## Package Contents

```
videotracker-ubuntu/
├── videotracker-linux      # Main executable binary
├── templates/              # HTML templates
│   ├── admin_standalone.html
│   └── watch_standalone.html
├── static/                 # Static assets
│   ├── images/
│   │   └── logo.png
│   └── videos/
├── videotracker.service    # Systemd service file
├── install.sh             # Automated installation script
├── DEPLOY.md              # This deployment guide
└── README.md              # Application documentation
```

## System Requirements

- **OS**: Ubuntu 18.04+ or Debian 10+ (x86_64)
- **RAM**: Minimum 128MB
- **Disk**: 50MB free space
- **Network**: Port 8080 (configurable)
- **Privileges**: Root/sudo access for installation

## Quick Installation

1. **Download and extract** the distribution package
2. **Run the installer** with root privileges:
   ```bash
   sudo ./install.sh
   ```

That's it! The service will be automatically configured and started.

## Manual Installation

If you prefer manual installation or need custom configuration:

### 1. Create Service User
```bash
sudo useradd --system --no-create-home --shell /bin/false videotracker
```

### 2. Create Installation Directory
```bash
sudo mkdir -p /opt/videotracker/{static/{images,videos},templates}
```

### 3. Copy Files
```bash
sudo cp videotracker-linux /opt/videotracker/videotracker
sudo cp -r templates/* /opt/videotracker/templates/
sudo cp -r static/* /opt/videotracker/static/
sudo chmod +x /opt/videotracker/videotracker
```

### 4. Set Permissions
```bash
sudo chown -R videotracker:videotracker /opt/videotracker
```

### 5. Install Systemd Service
```bash
sudo cp videotracker.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable videotracker
sudo systemctl start videotracker
```

## Configuration

### Environment Variables
The service accepts these environment variables (edit `/etc/systemd/system/videotracker.service`):

- `PORT`: Server port (default: 8080)

Example:
```ini
[Service]
Environment=PORT=9090
```

After changes, reload and restart:
```bash
sudo systemctl daemon-reload
sudo systemctl restart videotracker
```

### Database
- SQLite database is automatically created at `/opt/videotracker/video_tracker.db`
- No additional database setup required

## Usage

### Access Points
- **Admin Dashboard**: `http://your-server:8080/admin`
- **Video Watch Page**: `http://your-server:8080/watch?user_id=USER&video_id=VIDEO`
- **API Endpoints**: `http://your-server:8080/api/videos`

### Video Management
1. Access the admin dashboard
2. Use the "Add New Video" form to add videos with:
   - **Video ID**: Unique identifier
   - **Title**: Display name
   - **URL**: Direct URL to video file (S3, CDN, etc.)

### Watch Sessions
Users access videos via: `/watch?user_id=USERNAME&video_id=VIDEOID`

The system tracks:
- Entry/exit timestamps
- IP addresses
- Seconds watched (deduplicated)
- Total watch time

## Management Commands

```bash
# Service Management
sudo systemctl start videotracker      # Start service
sudo systemctl stop videotracker       # Stop service
sudo systemctl restart videotracker    # Restart service
sudo systemctl status videotracker     # Check status

# Logs
sudo journalctl -u videotracker -f     # Follow logs
sudo journalctl -u videotracker -n 100 # Last 100 lines

# Updates
sudo systemctl stop videotracker       # Stop service
sudo cp new-videotracker-linux /opt/videotracker/videotracker
sudo systemctl start videotracker      # Start service
```

## Firewall Configuration

### UFW (Ubuntu Firewall)
```bash
sudo ufw allow 8080/tcp
sudo ufw reload
```

### iptables
```bash
sudo iptables -A INPUT -p tcp --dport 8080 -j ACCEPT
sudo iptables-save > /etc/iptables/rules.v4
```

## Security Considerations

1. **Reverse Proxy**: Use nginx/Apache for HTTPS and domain mapping
2. **Firewall**: Restrict port 8080 to specific IPs if needed
3. **User Permissions**: Service runs as non-root `videotracker` user
4. **File Permissions**: Application directory has restricted access
5. **Updates**: Keep the binary updated for security patches

## Nginx Reverse Proxy Example

```nginx
server {
    listen 80;
    server_name your-domain.com;
    
    location / {
        proxy_pass http://localhost:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

## Troubleshooting

### Service Won't Start
```bash
# Check service status
sudo systemctl status videotracker

# View detailed logs
sudo journalctl -u videotracker -f

# Check binary permissions
ls -la /opt/videotracker/videotracker

# Test binary manually
sudo -u videotracker /opt/videotracker/videotracker
```

### Port Already in Use
```bash
# Check what's using port 8080
sudo lsof -i :8080

# Kill process or change port in service file
```

### Permission Issues
```bash
# Reset permissions
sudo chown -R videotracker:videotracker /opt/videotracker
sudo chmod +x /opt/videotracker/videotracker
```

### Database Issues
```bash
# Check database file
ls -la /opt/videotracker/video_tracker.db

# Reset database (deletes all data)
sudo rm /opt/videotracker/video_tracker.db
sudo systemctl restart videotracker
```

## Uninstallation

```bash
# Stop and disable service
sudo systemctl stop videotracker
sudo systemctl disable videotracker

# Remove service file
sudo rm /etc/systemd/system/videotracker.service
sudo systemctl daemon-reload

# Remove application files
sudo rm -rf /opt/videotracker

# Remove service user
sudo userdel videotracker
```

## Support

For issues, feature requests, or contributions, please visit:
https://github.com/lucastomic/videotracker