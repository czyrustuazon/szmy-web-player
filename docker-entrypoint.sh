#!/bin/sh
# Starts as root only to fix ownership of the data volume, then drops to PUID:PGID
# so files created through delete/upload keep the host user's ownership.
set -e

if [ "$(id -u)" != "0" ]; then
  exec "$@" # started with --user: nothing to do
fi

PUID="${PUID:-1000}"
PGID="${PGID:-1000}"
DATA="${MP_DATA_DIR:-/data}"

mkdir -p "$DATA"
if [ "$(stat -c %u:%g "$DATA")" != "$PUID:$PGID" ]; then
  chown -R "$PUID:$PGID" "$DATA"
fi

# The upload folder must be writable by the app user. It is usually a mount point that Docker
# just created as root, and this makes uploads work even when the rest of the library is
# read-only. Only this one folder is touched (not recursively). The music folder itself is
# deliberately left alone: if PUID:PGID cannot write to it, deleting is disabled.
case "$(echo "${MP_READ_ONLY:-}" | tr 'A-Z' 'a-z')" in
  1|true|yes|on) ;; # read-only on purpose: create nothing
  *)
    UP="${MP_MUSIC_DIR:-/music}/${MP_UPLOAD_SUBDIR:-uploads}"
    mkdir -p "$UP" 2>/dev/null || true
    if [ -d "$UP" ] && [ "$(stat -c %u:%g "$UP")" != "$PUID:$PGID" ]; then
      chown "$PUID:$PGID" "$UP" 2>/dev/null || true
    fi
    ;;
esac

exec setpriv --reuid="$PUID" --regid="$PGID" --clear-groups "$@"
