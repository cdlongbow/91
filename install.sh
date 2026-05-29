#!/usr/bin/env bash
set -Eeuo pipefail

APP_NAME="${APP_NAME:-video-site-91}"
GITHUB_REPO="${GITHUB_REPO:-nianzhibai/91}"
INSTALL_PATH="${INSTALL_PATH:-/opt/video-site-91}"
SERVICE_NAME="${SERVICE_NAME:-video-site-91}"
FRONTEND_PORT_WAS_SET="${FRONTEND_PORT+x}"
FRONTEND_PORT="${FRONTEND_PORT:-9191}"
VERSION="${VERSION:-latest}"
GH_PROXY="${GH_PROXY:-}"
CONFIGURE_UFW="${CONFIGURE_UFW:-1}"
INSTALL_DEPS="${INSTALL_DEPS:-1}"
SELF_UPDATE="${SELF_UPDATE:-1}"
INSTALL_SCRIPT_REF="${INSTALL_SCRIPT_REF:-main}"
INSTALL_SCRIPT_URL="${INSTALL_SCRIPT_URL:-${GH_PROXY}https://raw.githubusercontent.com/${GITHUB_REPO}/${INSTALL_SCRIPT_REF}/install.sh}"
VIDEO_SITE_SKIP_SELF_UPDATE="${VIDEO_SITE_SKIP_SELF_UPDATE:-0}"
VERSION_FILE="$INSTALL_PATH/.version"
MANAGER_PATH="/usr/local/sbin/${APP_NAME}-manager"
COMMAND_LINK="/usr/local/bin/91"
APP_COMMAND_LINK="/usr/local/bin/${APP_NAME}"

RED='\033[1;31m'
GREEN='\033[1;32m'
YELLOW='\033[1;33m'
BLUE='\033[1;34m'
RESET='\033[0m'

log() {
  printf "${BLUE}[install]${RESET} %s\n" "$*"
}

warn() {
  printf "${YELLOW}[install]${RESET} %s\n" "$*" >&2
}

die() {
  printf "${RED}[install]${RESET} %s\n" "$*" >&2
  exit 1
}

usage() {
  cat <<EOF
Usage:
  sudo bash install.sh [install]
  91 [update|restart|stop|status|logs|uninstall]

Default action:
  install.sh with no args downloads the prebuilt release package and starts the service.
  91 with no args opens the management menu.

Actions:
  install    Install to $INSTALL_PATH
  update     Refresh manager script, download latest release, and keep config/data
  restart    Restart service
  stop       Stop service
  status     Show service status
  logs       Follow service logs
  uninstall  Remove service and optionally delete installed files

Options via environment:
  GITHUB_REPO=$GITHUB_REPO
  VERSION=$VERSION              latest or a release tag such as v0.1.0
  INSTALL_PATH=$INSTALL_PATH
  FRONTEND_PORT=$FRONTEND_PORT
  GH_PROXY=$GH_PROXY
  INSTALL_DEPS=$INSTALL_DEPS
  CONFIGURE_UFW=$CONFIGURE_UFW
  SELF_UPDATE=$SELF_UPDATE
  INSTALL_SCRIPT_REF=$INSTALL_SCRIPT_REF
  INSTALL_SCRIPT_URL=$INSTALL_SCRIPT_URL

Examples:
  sudo bash install.sh
  FRONTEND_PORT=8080 sudo -E bash install.sh
  91
  91 update
  91 logs
EOF
}

is_manager_invocation() {
  local name
  name="$(basename "$0")"
  [[ "$name" == "91" || "$name" == "$APP_NAME" || "$name" == "$(basename "$MANAGER_PATH")" ]]
}

need_root() {
  if [[ "$(id -u)" == "0" ]]; then
    return
  fi
  if command -v sudo >/dev/null 2>&1; then
    exec sudo -E bash "$0" "$@"
  fi
  die "please run as root"
}

detect_arch() {
  local machine
  machine="$(uname -m)"
  case "$machine" in
    x86_64|amd64) ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *) die "unsupported architecture: $machine" ;;
  esac
}

download_base_url() {
  if [[ "$VERSION" == "latest" ]]; then
    printf '%shttps://github.com/%s/releases/latest/download' "$GH_PROXY" "$GITHUB_REPO"
  else
    printf '%shttps://github.com/%s/releases/download/%s' "$GH_PROXY" "$GITHUB_REPO" "$VERSION"
  fi
}

asset_name() {
  printf '%s-linux-%s.tar.gz' "$APP_NAME" "$ARCH"
}

install_deps() {
  if [[ "$INSTALL_DEPS" != "1" ]]; then
    return
  fi
  if command -v apt-get >/dev/null 2>&1; then
    export DEBIAN_FRONTEND=noninteractive
    log "installing runtime dependencies"
    apt-get update
    apt-get install -y ca-certificates curl tar ffmpeg openssl iproute2 python3 python3-requests python3-bs4 python3-lxml
    return
  fi

  for cmd in curl tar ffmpeg ffprobe openssl; do
    command -v "$cmd" >/dev/null 2>&1 || die "missing command: $cmd"
  done
}

check_system() {
  [[ "$(uname -s)" == "Linux" ]] || die "Linux is required"
  command -v systemctl >/dev/null 2>&1 || die "systemd is required"
  detect_arch
}

check_disk_space() {
  local parent avail
  parent="$(dirname "$INSTALL_PATH")"
  mkdir -p "$parent"
  avail="$(df -Pm "$parent" | awk 'NR==2 {print $4}')"
  if [[ "$avail" =~ ^[0-9]+$ ]] && (( avail < 512 )); then
    die "not enough free space under $parent, need at least 512 MB"
  fi
}

download_file() {
  local url="$1"
  local output="$2"
  local retry=0
  while (( retry < 3 )); do
    if curl -fL --connect-timeout 15 --retry 2 --retry-delay 2 "$url" -o "$output"; then
      [[ -s "$output" ]] && return 0
    fi
    retry=$((retry + 1))
    warn "download failed, retry $retry/3"
    sleep $((retry * 2))
  done
  return 1
}

backup_install_files() {
  local backup="$1"
  mkdir -p "$backup"
  cp -a "$INSTALL_PATH/server" "$backup/server"
  for item in dist config.example.yaml 91VideoSpider config.yaml .version; do
    if [[ -e "$INSTALL_PATH/$item" ]]; then
      cp -a "$INSTALL_PATH/$item" "$backup/$item"
    fi
  done
}

restore_install_files() {
  local backup="$1"
  mkdir -p "$INSTALL_PATH"
  cp -a "$backup/server" "$INSTALL_PATH/server"
  for item in dist config.example.yaml 91VideoSpider config.yaml .version; do
    rm -rf "${INSTALL_PATH:?}/$item"
    if [[ -e "$backup/$item" ]]; then
      cp -a "$backup/$item" "$INSTALL_PATH/$item"
    fi
  done
  chmod +x "$INSTALL_PATH/server"
}

prepare_config() {
  local cfg="$INSTALL_PATH/config.yaml"
  local example="$INSTALL_PATH/config.example.yaml"
  mkdir -p "$INSTALL_PATH/data"

  if [[ ! -f "$cfg" ]]; then
    cp "$example" "$cfg"
    sed -i -E "s#listen: \".*\"#listen: \"0.0.0.0:${FRONTEND_PORT}\"#" "$cfg"
    chmod 600 "$cfg"
    log "created $cfg"
  else
    log "keeping existing $cfg"
    if [[ -n "$FRONTEND_PORT_WAS_SET" ]]; then
      sed -i -E "s#listen: \".*\"#listen: \"0.0.0.0:${FRONTEND_PORT}\"#" "$cfg"
      log "updated listen port to ${FRONTEND_PORT}"
    fi
  fi

  if grep -q 'session_secret: "change-me-to-a-random-string"' "$cfg"; then
    local secret
    secret="$(openssl rand -hex 32)"
    sed -i -E "s#session_secret: \".*\"#session_secret: \"$secret\"#" "$cfg"
    log "generated random session_secret"
  fi
}

write_service() {
  cat >"/etc/systemd/system/${SERVICE_NAME}.service" <<EOF
[Unit]
Description=Video Site 91
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=${INSTALL_PATH}
ExecStart=${INSTALL_PATH}/server
Restart=on-failure
RestartSec=5
TimeoutStopSec=20
Environment=VIDEO_CONFIG=${INSTALL_PATH}/config.yaml
Environment=VIDEO_FRONTEND_DIR=${INSTALL_PATH}/dist
Environment=HOME=/root
Environment=PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin
LimitNOFILE=65536
StandardOutput=journal
StandardError=journal
SyslogIdentifier=${SERVICE_NAME}

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable "${SERVICE_NAME}.service" >/dev/null
}

install_cli() {
  local src
  src="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/$(basename "${BASH_SOURCE[0]}")"
  install_cli_from_file "$src"
}

install_cli_from_file() {
  local src="$1"
  local tmp
  [[ -f "$src" ]] || return 0
  mkdir -p "$(dirname "$MANAGER_PATH")" "$(dirname "$COMMAND_LINK")" "$(dirname "$APP_COMMAND_LINK")"
  tmp="${MANAGER_PATH}.tmp.$$"
  cp "$src" "$tmp"
  chmod 755 "$tmp"
  mv "$tmp" "$MANAGER_PATH"
  ln -sfn "$MANAGER_PATH" "$COMMAND_LINK"
  ln -sfn "$MANAGER_PATH" "$APP_COMMAND_LINK"
}

self_update_manager() {
  [[ "$SELF_UPDATE" == "1" ]] || return 1
  [[ "$VIDEO_SITE_SKIP_SELF_UPDATE" != "1" ]] || return 1
  [[ -n "$INSTALL_SCRIPT_URL" ]] || return 1

  local tmp
  tmp="$(mktemp)"
  log "checking latest manager script"
  if ! download_file "$INSTALL_SCRIPT_URL" "$tmp"; then
    warn "manager self-update skipped: cannot download $INSTALL_SCRIPT_URL"
    rm -f "$tmp"
    return 1
  fi
  if ! bash -n "$tmp"; then
    warn "manager self-update skipped: downloaded script has syntax errors"
    rm -f "$tmp"
    return 1
  fi
  if [[ -f "$MANAGER_PATH" ]] && cmp -s "$tmp" "$MANAGER_PATH"; then
    rm -f "$tmp"
    return 1
  fi

  install_cli_from_file "$tmp"
  rm -f "$tmp"
  log "manager script updated"
  return 0
}

exec_latest_manager_update() {
  local env_args=(
    "VIDEO_SITE_SKIP_SELF_UPDATE=1"
    "APP_NAME=$APP_NAME"
    "GITHUB_REPO=$GITHUB_REPO"
    "INSTALL_PATH=$INSTALL_PATH"
    "SERVICE_NAME=$SERVICE_NAME"
    "VERSION=$VERSION"
    "GH_PROXY=$GH_PROXY"
    "CONFIGURE_UFW=$CONFIGURE_UFW"
    "INSTALL_DEPS=$INSTALL_DEPS"
    "SELF_UPDATE=$SELF_UPDATE"
    "INSTALL_SCRIPT_REF=$INSTALL_SCRIPT_REF"
    "INSTALL_SCRIPT_URL=$INSTALL_SCRIPT_URL"
  )
  if [[ -n "$FRONTEND_PORT_WAS_SET" ]]; then
    env_args+=("FRONTEND_PORT=$FRONTEND_PORT")
  fi
  exec env "${env_args[@]}" bash "$MANAGER_PATH" update
}

open_firewall_port() {
  [[ "$CONFIGURE_UFW" == "1" ]] || return
  command -v ufw >/dev/null 2>&1 || return
  if ufw status 2>/dev/null | grep -qi "Status: active"; then
    log "allowing ${FRONTEND_PORT}/tcp in UFW"
    ufw allow "${FRONTEND_PORT}/tcp"
  fi
}

fetch_and_unpack() {
  local tmp archive url root
  tmp="$(mktemp -d)"
  archive="$tmp/$(asset_name)"
  url="$(download_base_url)/$(asset_name)"
  log "downloading $url"
  if ! download_file "$url" "$archive"; then
    warn "download failed: $url"
    rm -rf "$tmp"
    return 1
  fi

  if ! tar -xzf "$archive" -C "$tmp"; then
    warn "extract failed"
    rm -rf "$tmp"
    return 1
  fi
  root="$tmp/${APP_NAME}-linux-${ARCH}"
  if [[ ! -f "$root/server" || ! -d "$root/dist" || ! -f "$root/config.example.yaml" ]]; then
    warn "release package layout is invalid"
    rm -rf "$tmp"
    return 1
  fi

  mkdir -p "$INSTALL_PATH"
  cp "$root/server" "$INSTALL_PATH/server"
  rm -rf "$INSTALL_PATH/dist"
  cp -R "$root/dist" "$INSTALL_PATH/dist"
  cp "$root/config.example.yaml" "$INSTALL_PATH/config.example.yaml"
  if [[ -d "$root/91VideoSpider" ]]; then
    rm -rf "$INSTALL_PATH/91VideoSpider"
    cp -R "$root/91VideoSpider" "$INSTALL_PATH/91VideoSpider"
  fi
  chmod +x "$INSTALL_PATH/server"
  rm -rf "$tmp"
}

current_version_from_github() {
  if [[ "$VERSION" != "latest" ]]; then
    printf '%s' "$VERSION"
    return
  fi
  curl -fsSL "https://api.github.com/repos/${GITHUB_REPO}/releases/latest" \
    | sed -nE 's/.*"tag_name"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/p' \
    | head -n1
}

record_version() {
  local version
  version="$(current_version_from_github || true)"
  [[ -n "$version" ]] || version="$VERSION"
  {
    echo "$version"
    date '+%Y-%m-%d %H:%M:%S'
  } >"$VERSION_FILE"
}

show_success() {
  local local_ip public_ip version
  local_ip="$(ip addr show 2>/dev/null | awk '/inet / && $2 !~ /^127/ {sub(/\/.*/, "", $2); print $2; exit}')"
  public_ip="$(curl -s4 --connect-timeout 5 ip.sb 2>/dev/null || true)"
  version="$(head -n1 "$VERSION_FILE" 2>/dev/null || echo unknown)"

  echo
  printf '%b安装完成%b\n' "$GREEN" "$RESET"
  echo "版本：$version"
  [[ -n "$local_ip" ]] && echo "局域网：http://${local_ip}:${FRONTEND_PORT}/"
  [[ -n "$public_ip" ]] && echo "公网：  http://${public_ip}:${FRONTEND_PORT}/"
  echo "后台：  http://服务器IP:${FRONTEND_PORT}/admin"
  echo "数据：  $INSTALL_PATH/data"
  echo
  echo "首次访问后台时会要求设置管理员用户名和密码。"
  echo "管理命令：91 或 91 status | logs | update | restart | stop"
}

install_app() {
  check_system
  check_disk_space
  install_deps
  systemctl stop "${SERVICE_NAME}.service" 2>/dev/null || true
  fetch_and_unpack || die "install failed"
  prepare_config
  write_service
  install_cli
  open_firewall_port
  record_version
  systemctl restart "${SERVICE_NAME}.service"
  show_success
}

update_app() {
  check_system
  check_disk_space
  install_deps
  [[ -f "$INSTALL_PATH/server" ]] || die "not installed at $INSTALL_PATH"

  if self_update_manager; then
    log "re-running update with latest manager script"
    exec_latest_manager_update
  fi

  local backup
  backup="$(mktemp -d)"
  backup_install_files "$backup"

  systemctl stop "${SERVICE_NAME}.service" 2>/dev/null || true
  if ! (fetch_and_unpack && prepare_config && write_service && install_cli); then
    warn "update failed; restoring previous files"
    restore_install_files "$backup"
    systemctl start "${SERVICE_NAME}.service" 2>/dev/null || true
    rm -rf "$backup"
    exit 1
  fi

  if ! systemctl restart "${SERVICE_NAME}.service"; then
    warn "new version failed to start; restoring previous files"
    restore_install_files "$backup"
    systemctl restart "${SERVICE_NAME}.service" 2>/dev/null || true
    rm -rf "$backup"
    exit 1
  fi
  record_version
  rm -rf "$backup"
  log "updated"
}

uninstall_app() {
  systemctl disable --now "${SERVICE_NAME}.service" 2>/dev/null || true
  rm -f "/etc/systemd/system/${SERVICE_NAME}.service"
  systemctl daemon-reload
  rm -f "$COMMAND_LINK" "$APP_COMMAND_LINK" "$MANAGER_PATH"

  if [[ -t 0 ]]; then
    read -r -p "删除 $INSTALL_PATH 里的程序、配置和数据吗？[y/N]: " confirm
    case "$confirm" in
      [yY]) rm -rf "$INSTALL_PATH" ;;
      *) log "kept $INSTALL_PATH" ;;
    esac
  else
    log "removed service; kept $INSTALL_PATH"
  fi
}

show_menu() {
  if [[ ! -t 0 ]]; then
    usage
    return 0
  fi

  while true; do
    clear
    echo "欢迎使用 91 管理脚本"
    echo
    echo "基础功能："
    echo "1、查看状态"
    echo "2、查看日志"
    echo "3、更新 91"
    echo "4、重启 91"
    echo "5、停止 91"
    echo "6、卸载 91"
    echo "0、退出"
    echo
    read -r -p "请输入选项 [0-6]: " choice

    case "$choice" in
      1) main status ;;
      2) main logs ;;
      3) main update ;;
      4) main restart ;;
      5) main stop ;;
      6) main uninstall ;;
      0) exit 0 ;;
      *) echo "无效的选项" ;;
    esac

    echo
    read -r -n1 -s -p "按任意键继续 ..."
  done
}

main() {
  local action="${1:-}"
  if [[ -z "$action" ]]; then
    if is_manager_invocation; then
      show_menu
      return
    fi
    action="install"
  fi

  case "$action" in
    install)
      need_root "$@"
      install_app
      ;;
    update)
      need_root "$@"
      update_app
      ;;
    restart)
      need_root "$@"
      systemctl restart "${SERVICE_NAME}.service"
      ;;
    stop)
      need_root "$@"
      systemctl stop "${SERVICE_NAME}.service"
      ;;
    status)
      systemctl --no-pager --full status "${SERVICE_NAME}.service" || true
      ;;
    logs)
      journalctl -u "${SERVICE_NAME}.service" -f
      ;;
    menu)
      show_menu
      ;;
    uninstall)
      need_root "$@"
      uninstall_app
      ;;
    -h|--help|help)
      usage
      ;;
    *)
      usage >&2
      exit 2
      ;;
  esac
}

main "$@"
