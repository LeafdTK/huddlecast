#!/bin/bash
set -uo pipefail

mkdir -p /data /recordings
chown -R huddlecast:huddlecast /data /recordings 2>/dev/null || true

as_app() { setpriv --reuid=10001 --regid=10001 --init-groups -- "$@"; }

as_app mediamtx "${MEDIAMTX_CONFIG:-/config/mediamtx.yml}" &
mtx=$!

for _ in $(seq 1 60); do
  curl -sf -o /dev/null "http://127.0.0.1:9997/v3/config/global/get" && break
  sleep 0.25
done

as_app huddlecast serve --config "${HUDDLECAST_CONFIG:-/config/huddlecast.yml}" &
app=$!

shutdown() { kill -TERM "$mtx" "$app" 2>/dev/null; }
trap shutdown TERM INT

wait -n "$mtx" "$app"
code=$?
shutdown
wait 2>/dev/null
exit "$code"
