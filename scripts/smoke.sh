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

if [ "$(docker inspect -f '{{.Architecture}}' "$IMG")" = "amd64" ]; then
  docker run --rm --entrypoint vgmstream-cli "$IMG" -h >/dev/null 2>&1 || true
  docker run --rm --entrypoint sh "$IMG" -c 'test -x /usr/local/bin/vgmstream-cli'
  echo "vgmstream-cli present"
fi

echo "smoke test passed"
