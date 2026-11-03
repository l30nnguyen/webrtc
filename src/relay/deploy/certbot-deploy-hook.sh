#!/bin/sh
# certbot deploy hook: /etc/letsencrypt/renewal-hooks/deploy/matrix-relay.sh
# Copies the renewed certificate where the relay user can read it; the relay
# picks it up within a minute without a restart. Set DOMAIN below.
set -eu
DOMAIN=webrtc.5gen.care
src=/etc/letsencrypt/live/$DOMAIN
dst=/etc/matrix-relay/tls
install -d -m 0750 -o root -g matrix-relay "$dst"
install -m 0640 -o root -g matrix-relay "$src/privkey.pem" "$dst/privkey.pem.new"
install -m 0644 -o root -g matrix-relay "$src/fullchain.pem" "$dst/fullchain.pem.new"
mv "$dst/privkey.pem.new" "$dst/privkey.pem"
mv "$dst/fullchain.pem.new" "$dst/fullchain.pem"
