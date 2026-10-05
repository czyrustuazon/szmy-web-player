#!/bin/sh
# End-to-end upload test: builds the image, starts it with an empty library and a SEPARATE
# uploads mount (what UPLOADS_HOST_PATH does), builds real
# fixtures (a 40 MiB file, a .zip and a .7z) inside the image and runs the browser upload
# client against the server (scripts/e2e-upload.mjs, in a node container).
# Needs docker. Run: sh scripts/e2e.sh
set -eu

IMG="masterplayer:e2e"
PORT="${E2E_PORT:-18199}"
NAME="mp-e2e-$$"

docker build -t "$IMG" .

tmp=$(mktemp -d)
mkdir -p "$tmp/music" "$tmp/data" "$tmp/fixtures" "$tmp/uploads"
echo '{"type":"module"}' > "$tmp/package.json"
cleanup() { docker rm -f "$NAME" >/dev/null 2>&1 || true; docker run --rm -v "$tmp:/t" --entrypoint sh "$IMG" -c 'rm -rf /t/* /t/.[!.]*' >/dev/null 2>&1 || true; rm -rf "$tmp" 2>/dev/null || true; }
trap cleanup EXIT

# Mimic a fresh server where Docker created the mount folders as root: the library root is then
# not writable for the app user (deleting is off), but uploads must still work.
docker run --rm -v "$tmp:/t" --entrypoint chown "$IMG" -R 0:0 /t/music /t/uploads

# Fixtures, made with the tools inside the image (7z is what the server uses too).
docker run --rm -v "$tmp/fixtures:/f" --entrypoint sh "$IMG" -c '
  set -e
  cd /f
  mkdir -p src/Album/CD2
  mk() { printf "ID3\003\000\000\000\000\000\000" > "$1"; head -c "$2" /dev/urandom >> "$1"; }
  mk big.mp3 41943040
  mk src/Album/01.mp3 300000; mk src/Album/02.mp3 300000; mk src/Album/CD2/03.mp3 300000
  echo notes > src/Album/readme.txt
  printf "\211PNG\r\n" > src/Album/cover.png          # not a real picture: dropped
  printf "\377\330\377\340 a small picture" > src/Album/cover.jpg   # a real one: kept as cover art
  printf "#!/bin/sh\nrm -rf /\n" > src/Album/run.sh
  ln -s /etc/passwd src/Album/link.mp3
  7z a -tzip -snl album.zip src/Album >/dev/null
  7z a -snl album.7z src/Album >/dev/null
  # Formats browsers cannot play, made by the ffmpeg inside the image (it has no APE encoder).
  mkdir -p ff
  tone="-f lavfi -i sine=frequency=440:duration=2"
  tags="-metadata title=Windows -metadata artist=Redmond"
  ffmpeg -v error $tone $tags -c:a wmav2 ff/song.wma
  ffmpeg -v error $tone $tags -c:a wavpack ff/song.wv
  ffmpeg -v error $tone $tags -c:a flac ff/song.mka
  ffmpeg -v error $tone $tags -c:a libmp3lame ff/song.flv
  chmod -R a+rX /f
'

docker run -d --name "$NAME" -p "${PORT}:8080" -e PUID="$(id -u)" -e PGID="$(id -g)" \
  -v "$tmp/music:/music" -v "$tmp/uploads:/music/uploads" -v "$tmp/data:/data" "$IMG" >/dev/null
i=0
until curl -fsS "http://127.0.0.1:${PORT}/healthz" >/dev/null 2>&1; do
  i=$((i + 1)); [ "$i" -gt 30 ] && { echo "server did not start"; docker logs "$NAME"; exit 1; }
  sleep 1
done

docker run --rm --network host -e E2E_URL="http://127.0.0.1:${PORT}" \
  -v "$tmp/package.json:/e2e/package.json:ro" \
  -v "$PWD/web/js:/e2e/js:ro" \
  -v "$PWD/scripts/e2e-upload.mjs:/e2e/e2e-upload.mjs:ro" \
  -v "$tmp/fixtures:/e2e/fixtures:ro" \
  -v "$tmp/music:/e2e/music:ro" -v "$tmp/uploads:/e2e/music/uploads:ro" \
  -v "$tmp/uploads:/e2e/uploads-host:ro" \
  -w /e2e node:22-slim node e2e-upload.mjs
