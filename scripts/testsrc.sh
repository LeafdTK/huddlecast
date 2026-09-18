#!/usr/bin/env sh
set -eu
KEY="${1:-}"
HOST="${MEDIAMTX_HOST:-127.0.0.1}"
if [ -z "$KEY" ]; then
  echo "usage: $0 STREAM_KEY   (env MEDIAMTX_HOST to override host)"; exit 1
fi
exec ffmpeg -re -f lavfi -i "testsrc2=size=1280x720:rate=30" -f lavfi -i "sine=frequency=440:sample_rate=48000" \
  -c:v libx264 -preset veryfast -tune zerolatency -pix_fmt yuv420p -g 60 -b:v 2500k \
  -c:a aac -b:a 128k -ar 48000 -ac 2 \
  -f flv "rtmp://$HOST:1935/rtmp/$KEY"
