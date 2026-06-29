#!/bin/bash
# =============================================================================
# WebRTC Signaling Server Setup: nginx + certbot + signaling-server.py
# Ubuntu 22.04 / 24.04 on AWS EC2
# Usage: sudo bash setup-webrtc.sh <your-domain.com> <your-email@example.com>
# =============================================================================

set -e  # exit on any error

DOMAIN=$1
EMAIL=$2

# --- Validate inputs ---------------------------------------------------------
if [ -z "$DOMAIN" ] || [ -z "$EMAIL" ]; then
    exit 1
fi

echo "============================================"
echo " Domain : $DOMAIN"
echo " Email  : $EMAIL"
echo "============================================"

# =============================================================================
# 1. System update + install dependencies
# =============================================================================
echo ""
echo "[1/6] Installing nginx, certbot, python3 dependencies..."

apt-get update -y
apt-get install -y nginx certbot python3-certbot-nginx python3-pip python3-venv

# =============================================================================
# 2. Copy ws.html to web root
# =============================================================================
echo ""
echo "[2/6] Setting up web root..."

mkdir -p /var/www/webrtc
cp "player/ws.html" /var/www/webrtc/ws.html
chown -R www-data:www-data /var/www/webrtc

# =============================================================================
# 3. Setup signaling server as a systemd service
# =============================================================================
echo ""
echo "[3/6] Run pm2 start pm2_start.json"

# =============================================================================
# 4. Configure nginx (HTTP first, for certbot challenge)
# =============================================================================
echo ""
echo "[4/6] Configuring nginx..."

# Temporary HTTP config for certbot domain validation
cat > /etc/nginx/sites-available/webrtc << EOF
server {
    listen 80;
    server_name $DOMAIN;

    # Certbot will use this for the ACME challenge
    location /.well-known/acme-challenge/ {
        root /var/www/html;
    }

    # Redirect everything else to HTTPS
    location / {
        return 301 https://\$host\$request_uri;
    }
}
EOF

ln -sf /etc/nginx/sites-available/webrtc /etc/nginx/sites-enabled/webrtc

# Remove default site if present
rm -f /etc/nginx/sites-enabled/default

nginx -t && systemctl reload nginx
echo "  nginx configured for HTTP (temporary, for cert issuance)"

# =============================================================================
# 5. Obtain SSL certificate via certbot
# =============================================================================
echo ""
echo "[5/6] Obtaining SSL certificate from Let's Encrypt..."

certbot certonly \
    --nginx \
    --non-interactive \
    --agree-tos \
    --email "$EMAIL" \
    -d "$DOMAIN"

echo "  Certificate obtained: /etc/letsencrypt/live/$DOMAIN/"

# Now write the full HTTPS config with WebSocket proxy
cat > /etc/nginx/sites-available/webrtc << EOF
# HTTP -> HTTPS redirect
server {
    listen 80;
    server_name $DOMAIN;
    return 301 https://\$host\$request_uri;
}

# Main HTTPS server
server {
    listen 443 ssl;
    server_name $DOMAIN;

    ssl_certificate     /etc/letsencrypt/live/$DOMAIN/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/$DOMAIN/privkey.pem;
    ssl_protocols       TLSv1.2 TLSv1.3;
    ssl_ciphers         HIGH:!aNULL:!MD5;

    # Serve ws.html at /ws (and / serves it too)
    root /var/www/webrtc;
    index ws.html;

    # Serve static files (ws.html)
    location = / {
        try_files /ws.html =404;
    }

    location = /ws {
        try_files /ws.html =404;
    }

    # All other paths: proxy to signaling server as WebSocket
    location / {
        proxy_pass         http://localhost:8000;
        proxy_http_version 1.1;
        proxy_set_header   Upgrade $http_upgrade;
        proxy_set_header   Connection "upgrade";
        proxy_set_header   Host $host;
        proxy_set_header   X-Real-IP $remote_addr;
        proxy_read_timeout 3600s;
        proxy_send_timeout 3600s;
    }
}
EOF

nginx -t && systemctl reload nginx
echo "  nginx configured for HTTPS + WebSocket proxy"

# =============================================================================
# 6. Auto-renew cert (certbot installs a timer, but confirm it)
# =============================================================================
echo ""
echo "[6/6] Verifying certbot auto-renewal..."

systemctl enable certbot.timer 2>/dev/null || true
# Test renewal dry-run
certbot renew --dry-run --quiet && echo "  Auto-renewal OK"

# =============================================================================
# Done
# =============================================================================
echo ""
echo "============================================"
echo " SETUP COMPLETE"
echo "============================================"