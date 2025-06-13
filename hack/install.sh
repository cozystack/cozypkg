#!/usr/bin/env bash
set -euo pipefail
trap 'error "An unexpected error occurred."; exit 1' ERR

info()    { printf "\033[1;34m[INFO]\033[0m %s\n" "$*"; }
success() { printf "\033[1;32m[SUCCESS]\033[0m %s\n" "$*"; }
warn()    { printf "\033[1;33m[WARN]\033[0m %s\n" "$*" >&2; }
error()   { printf "\033[1;31m[ERROR]\033[0m %s\n" "$*" >&2; }

for cmd in curl uname mktemp jq tar; do
  if ! command -v "$cmd" >/dev/null 2>&1; then
    error "Required command '$cmd' not found. Please install it and retry."
    exit 1
  fi
done

info "Fetching latest cozypkg release version..."

API_URL="https://api.github.com/repos/cozystack/cozypkg/releases/latest"
TMPDIR=$(mktemp -d)
cleanup() { rm -rf "$TMPDIR"; }
trap cleanup EXIT

HTTP_STATUS=$(curl -sSL -w '%{http_code}' -H 'Accept: application/vnd.github.v3+json' -o "$TMPDIR/response.json" "$API_URL")

if [ "$HTTP_STATUS" -ne 200 ]; then
  error "GitHub API returned HTTP status $HTTP_STATUS."
  exit 1
fi

LATEST_VERSION=$(jq -r '.tag_name' < "$TMPDIR/response.json")
if [ -z "$LATEST_VERSION" ] || [ "$LATEST_VERSION" = "null" ]; then
  error "Could not parse 'tag_name' from GitHub release response."
  exit 1
fi

info "Latest version: $LATEST_VERSION"

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)

case "$ARCH" in
  x86_64 | amd64) ARCH="amd64" ;;
  arm64 | aarch64) ARCH="arm64" ;;
  i386 | i686) ARCH="i386" ;;
  *)
    error "Unsupported architecture: $ARCH"
    exit 1
    ;;
esac

TAR_FILE="cozypkg-$OS-$ARCH.tar.gz"
DOWNLOAD_URL="https://github.com/cozystack/cozypkg/releases/download/$LATEST_VERSION/$TAR_FILE"

info "Downloading $TAR_FILE from $DOWNLOAD_URL..."
curl -fL "$DOWNLOAD_URL" -o "$TMPDIR/$TAR_FILE"

info "Extracting..."
tar -xzf "$TMPDIR/$TAR_FILE" -C "$TMPDIR"

if ! [ -f "$TMPDIR/cozypkg" ]; then
  error "Expected binary 'cozypkg' not found in archive."
  exit 1
fi

chmod +x "$TMPDIR/cozypkg"

if [ "$(id -u)" = 0 ]; then
  INSTALL_DIR="/usr/local/bin"
elif [ -w "/usr/local/bin" ]; then
  INSTALL_DIR="/usr/local/bin"
else
  INSTALL_DIR="$HOME/.local/bin"
  mkdir -p "$INSTALL_DIR"
  case ":$PATH:" in
    *":$INSTALL_DIR:"*) ;;
    *) warn "$INSTALL_DIR is not in your PATH." ;;
  esac
fi

INSTALL_PATH="$INSTALL_DIR/cozypkg"
mv "$TMPDIR/cozypkg" "$INSTALL_PATH"

success "cozypkg installed successfully at $INSTALL_PATH"
info "You can now run: cozypkg --help"
