#!/bin/sh
set -eu

APP_DIR="/opt/video-site-91"
DATA_DIR="${VIDEO_DATA_DIR:-$APP_DIR/data}"
CONFIG="${VIDEO_CONFIG:-$DATA_DIR/config.yaml}"
EXAMPLE="$APP_DIR/config.example.yaml"
PORT="${VIDEO_LISTEN_PORT:-9191}"

mkdir -p "$DATA_DIR" "$DATA_DIR/previews" "$DATA_DIR/uploads" "$DATA_DIR/spider91"

if [ ! -f "$CONFIG" ]; then
  if [ ! -f "$EXAMPLE" ]; then
    echo "[entrypoint] missing config template: $EXAMPLE" >&2
    exit 1
  fi

  mkdir -p "$(dirname "$CONFIG")"
  cp "$EXAMPLE" "$CONFIG"

  SECRET="$(openssl rand -hex 32)"
  sed -i -E "s#^([[:space:]]*listen:[[:space:]]*).*\$#\1\"0.0.0.0:${PORT}\"#" "$CONFIG"
  sed -i -E "s#^([[:space:]]*session_secret:[[:space:]]*).*\$#\1\"${SECRET}\"#" "$CONFIG"
  sed -i -E "s#^([[:space:]]*db_path:[[:space:]]*).*\$#\1\"${DATA_DIR}/video-site.db\"#" "$CONFIG"
  sed -i -E "s#^([[:space:]]*local_preview_dir:[[:space:]]*).*\$#\1\"${DATA_DIR}/previews\"#" "$CONFIG"
  chmod 600 "$CONFIG"

  echo "[entrypoint] generated $CONFIG"
else
  echo "[entrypoint] using existing $CONFIG"
fi

if [ -n "${VIDEO_VERSION_FILE:-}" ] && [ -n "${VIDEO_IMAGE_VERSION:-}" ] && [ ! -f "$VIDEO_VERSION_FILE" ]; then
  mkdir -p "$(dirname "$VIDEO_VERSION_FILE")"
  printf '%s\n' "$VIDEO_IMAGE_VERSION" > "$VIDEO_VERSION_FILE"
fi

exec "$@"
