#!/bin/bash

cp player/ws.html /var/www/webrtc/ws.html
chown -R www-data:www-data /var/www/webrtc

cp downloads/* /var/www/downloads/
chown -R www-data:www-data /var/www/downloads
