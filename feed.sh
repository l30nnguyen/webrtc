#!/bin/bash

ffmpeg -f avfoundation -pixel_format uyvy422 -r 30 -i "2" \
  -vf "format=yuv420p,scale=1280:720" \
  -vcodec libx264 \
  -profile:v main \
  -level 4.1 \
  -preset veryfast \
  -g 30 \
  -an -f h264 udp://127.0.0.1:8553