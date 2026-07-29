#!/bin/bash

# Get the directory where this script is located
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Certificate Renewal Script for WebRTC Signaling Server
# This script checks if the SSL certificate is expired or expiring soon,
# and renews it using certbot if needed.
#
# CRONJOB SETUP:
# To run this script daily at 00:00:00, add to crontab:
#
#   sudo crontab -e
#
# Then add this line:
#   0 0 * * * /home/leon/code/webrtc/server/renewcert.sh >> /var/log/webrtc-cert-renewal.log 2>&1
#
# This will:
# - Run at 00:00 (midnight) every day
# - Log output to /var/log/webrtc-cert-renewal.log
# - Run as root (required for certbot)

# Configuration
DOMAIN="webrtc.5gen.care"
CERT_PATH="${SCRIPT_DIR}/cert.pem"
CERT_DIR="/etc/letsencrypt/live/${DOMAIN}"
DAYS_BEFORE_EXPIRY=30  # Renew if cert expires within this many days

# Check if cert exists
if [ ! -f "$CERT_PATH" ]; then
    echo "$(date): Certificate not found at $CERT_PATH. Generating new certificate..."
    
    # Generate new certificate using certbot standalone mode
    # Standalone mode requires port 80 to be free
    certbot certonly --standalone --non-interactive --agree-tos --email admin@${DOMAIN} -d ${DOMAIN}
    
    if [ $? -eq 0 ]; then
        echo "$(date): Certificate generated successfully"
    else
        echo "$(date): ERROR: Failed to generate certificate"
        exit 1
    fi
else
    # Check if certificate is expired or expiring soon
    EXPIRY_DATE=$(openssl x509 -enddate -noout -in "$CERT_DIR/fullchain.pem" | cut -d= -f2)
    EXPIRY_EPOCH=$(date -d "$EXPIRY_DATE" +%s)
    CURRENT_EPOCH=$(date +%s)
    DAYS_LEFT=$(( (EXPIRY_EPOCH - CURRENT_EPOCH) / 86400 ))
    
    echo "$(date): Certificate expires on $EXPIRY_DATE ($DAYS_LEFT days left)"
    
    if [ $DAYS_LEFT -lt $DAYS_BEFORE_EXPIRY ]; then
        echo "$(date): Certificate expiring soon. Renewing..."
        
        # Stop the Go server temporarily to free port 8443 if needed
        # Note: certbot standalone only needs port 80, not 8443
        # But if you're using webroot mode, uncomment the next lines:
        # pm2 stop webrtc_prod
        
        # Renew certificate
        certbot renew --non-interactive
        
        if [ $? -eq 0 ]; then
            echo "$(date): Certificate renewed successfully"
            
            # Restart the Go server if you stopped it
            # pm2 start webrtc_prod
        else
            echo "$(date): ERROR: Failed to renew certificate"
            exit 1
        fi
    else
        echo "$(date): Certificate is still valid. No renewal needed."
        exit 0
    fi
fi

# Combine fullchain and private key into cert.pem
echo "$(date): Updating $CERT_PATH..."
cat "$CERT_DIR/fullchain.pem" "$CERT_DIR/privkey.pem" > "$CERT_PATH"

# Set proper permissions
chmod 644 "$CERT_PATH"

# Restart the Go server to load the new certificate
echo "$(date): Restarting Go server..."
pm2 restart webrtc_prod

if [ $? -eq 0 ]; then
    echo "$(date): Go server restarted successfully"
else
    echo "$(date): ERROR: Failed to restart Go server"
    exit 1
fi

echo "$(date): Certificate renewal process completed successfully"
