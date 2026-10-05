#!/bin/sh
# Builds the image, starts it with a fixture library and checks the main paths.
# Needs docker and curl. Run: sh scripts/smoke.sh
set -eu

IMG="masterplayer:smoke"
PORT="${SMOKE_PORT:-18080}"
BASE="http://127.0.0.1:${PORT}"
H='X-Requested-With: masterplayer'

docker build -t "$IMG" .

tmp=$(mktemp -d)
mkdir -p "$tmp/music" "$tmp/data"
# Smallest thing the content sniffer accepts as MP3 (ID3 header + frame sync).
printf 'ID3\003\000\000\000\000\000\000\377\373\220\000' > "$tmp/music/test.mp3"

cid=$(docker run -d -p "${PORT}:8080" \
  -e MP_ADMIN_PASSWORD=smoke -e PUID="$(id -u)" -e PGID="$(id -g)" \
  -v "$tmp/music:/music" -v "$tmp/data:/data" "$IMG")
cleanup() { docker rm -f "$cid" >/dev/null 2>&1 || true; rm -rf "$tmp"; }
trap cleanup EXIT

i=0
until curl -fsS "$BASE/healthz" >/dev/null 2>&1; do
  i=$((i + 1))
  [ "$i" -gt 30 ] && { echo "server did not become healthy"; docker logs "$cid"; exit 1; }
  sleep 1
done
echo "healthz ok"

# Unauthenticated access is refused.
code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/browse")
[ "$code" = "401" ] || { echo "expected 401 without login, got $code"; exit 1; }

curl -fsS -c "$tmp/cookies" -H "$H" -H 'Content-Type: application/json' \
  -d '{"password":"smoke"}' "$BASE/api/login" >/dev/null
echo "login ok"

curl -fsS -b "$tmp/cookies" "$BASE/api/browse" | grep -q 'test.mp3'
echo "browse ok"

code=$(curl -s -b "$tmp/cookies" -H 'Range: bytes=0-3' -o /dev/null -w '%{http_code}' "$BASE/api/stream?p=test.mp3")
[ "$code" = "206" ] || { echo "expected 206 for a range request, got $code"; exit 1; }
echo "range streaming ok"

# Blank password means open access: no login needed.
OPEN_PORT=$((PORT + 1))
ocid=$(docker run -d -p "${OPEN_PORT}:8080" -e PUID="$(id -u)" -e PGID="$(id -g)" \
  -v "$tmp/music:/music" -v "$tmp/data-open:/data" "$IMG")
trap 'docker rm -f "$cid" "$ocid" >/dev/null 2>&1 || true; rm -rf "$tmp"' EXIT
i=0
until curl -fsS "http://127.0.0.1:${OPEN_PORT}/healthz" >/dev/null 2>&1; do
  i=$((i + 1))
  [ "$i" -gt 30 ] && { echo "open-access server did not become healthy"; docker logs "$ocid"; exit 1; }
  sleep 1
done
curl -fsS "http://127.0.0.1:${OPEN_PORT}/api/browse" | grep -q 'test.mp3'
docker logs "$ocid" 2>&1 | grep -q 'NO LOGIN'
echo "open access (blank password) ok"

# Access control: with only Tailscale allowed, a request from the Docker host (a private
# address) is refused with 403, while the health check from inside the container (loopback)
# still passes. A forged X-Forwarded-For must not change anything.
DENY_PORT=$((PORT + 2))
dcid=$(docker run -d -p "${DENY_PORT}:8080" -e MP_ALLOWED_NETS=tailscale -e PUID="$(id -u)" -e PGID="$(id -g)"   -v "$tmp/music:/music" -v "$tmp/data-deny:/data" "$IMG")
trap 'docker rm -f "$cid" "$ocid" "$dcid" >/dev/null 2>&1 || true; rm -rf "$tmp"' EXIT
sleep 3
code=$(curl -s -o /dev/null -w '%{http_code}' -H 'X-Forwarded-For: 100.114.200.30' "http://127.0.0.1:${DENY_PORT}/api/session")
[ "$code" = "403" ] || { echo "expected 403 from a refused address, got $code"; docker logs "$dcid"; exit 1; }
docker exec "$dcid" masterplayer -healthcheck || { echo "the in-container health check must still pass"; exit 1; }
docker logs "$dcid" 2>&1 | grep -q 'access: refused'
echo "access control ok (refused address gets 403, health check still passes)"

if [ "$(docker inspect -f '{{.Architecture}}' "$IMG")" = "amd64" ]; then
  docker run --rm --entrypoint vgmstream-cli "$IMG" -h >/dev/null 2>&1 || true
  docker run --rm --entrypoint sh "$IMG" -c 'test -x /usr/local/bin/vgmstream-cli'
  echo "vgmstream-cli present"
fi

docker run --rm --entrypoint sh "$IMG" -c 'command -v ffmpeg && command -v ffprobe' >/dev/null
echo "ffmpeg and ffprobe present"

echo "smoke test passed"
