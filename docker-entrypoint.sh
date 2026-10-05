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

# The music folder is deliberately left alone. If PUID:PGID cannot write to it the app
# detects that and runs read-only (delete and upload are disabled).
exec setpriv --reuid="$PUID" --regid="$PGID" --clear-groups "$@"
