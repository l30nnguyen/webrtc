# ===== Dockerfile =====
FROM node:18-alpine

WORKDIR /app

# Install dependencies
COPY package*.json ./
RUN npm install --production

# Copy application files
COPY server.js ./
COPY client.html ./

# Expose ports
EXPOSE 1988
EXPOSE 8554/udp

# Health check
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD node -e "require('http').get('http://localhost:1988/health', (r) => { process.exit(r.statusCode === 200 ? 0 : 1); })"

CMD ["node", "server.js"]

# ===== docker-compose.yml =====
# Save this as a separate file: docker-compose.yml
#
# version: '3.8'
#
# services:
#   webrtc-server:
#     build: .
#     ports:
#       - "1988:1988"
#       - "8554:8554/udp"
#     environment:
#       - NODE_ENV=production
#     restart: unless-stopped
#     networks:
#       - webrtc-net
#
#   # Optional: Add Coturn TURN server for production
#   coturn:
#     image: coturn/coturn:latest
#     ports:
#       - "3478:3478/udp"
#       - "3478:3478/tcp"
#     command:
#       - "-n"
#       - "--log-file=stdout"
#       - "--external-ip=$$(detect-external-ip)"
#       - "--listening-port=3478"
#       - "--realm=webrtc"
#       - "--user=admin:admin"
#       - "--lt-cred-mech"
#     restart: unless-stopped
#     networks:
#       - webrtc-net
#
# networks:
#   webrtc-net:
#     driver: bridge

# ===== .dockerignore =====
# node_modules
# npm-debug.log
# .git
# .gitignore
# README.md
# test-udp-sender.js

# ===== Build & Run Commands =====
#
# Build:
#   docker build -t webrtc-h264-streamer .
#
# Run:
#   docker run -d \
#     -p 1988:1988 \
#     -p 8554:8554/udp \
#     --name webrtc-streamer \
#     webrtc-h264-streamer
#
# With Docker Compose:
#   docker-compose up -d
#
# View logs:
#   docker logs -f webrtc-streamer
#
# Stop:
#   docker stop webrtc-streamer
#   # or
#   docker-compose down