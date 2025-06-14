#!/usr/bin/env sh
set -e

info()    { printf "\033[1;34m[INFO]\033[0m %s\n" "$*"; }
success() { printf "\033[1;32m[SUCCESS]\033[0m %s\n" "$*"; }
warn()    { printf "\033[1;33m[WARN]\033[0m %s\n" "$*" >&2; }
error()   { printf "\033[1;31m[ERROR]\033[0m %s\n" "$*" >&2; exit 1; }

# ----------------------
# Argument parsing
# ----------------------
VERSION="latest"

usage() {
  cat <<EOF
Usage: $0 [-v VERSION]

Options:
  -v VERSION  Install a specific release tag (e.g. 1.3.0 or v1.3.0). Defaults to latest.
  -h          Show this help message and exit.
EOF
}

while getopts "v:h" opt; do
  case $opt in
    v) VERSION="$OPTARG" ;;
    h) usage; exit 0 ;;
    *) usage; exit 1 ;;
  esac
done
shift $((OPTIND-1))

# Normalize tag: prepend 'v' if user omitted it
if [ "$VERSION" != "latest" ]; then
  case "$VERSION" in
    v*) TAG="$VERSION" ;;
    *)  TAG="v$VERSION" ;;
  esac
else
  TAG="latest"
fi

# ----------------------
# Prerequisite commands
# ----------------------
for cmd in uname mktemp tar sha256sum; do
  command -v "$cmd" >/dev/null 2>&1 || error "Required command '$cmd' not found."
done

# Detect download tool
if command -v curl >/dev/null 2>&1; then
  download() { curl -fsSL -o "$1" "$2"; }
elif command -v wget >/dev/null 2>&1; then
  download() { wget -qO "$1" "$2"; }
else
  error "Neither curl nor wget is available."
fi

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)

case "$ARCH" in
  x86_64|amd64) ARCH="amd64" ;;
  arm64|aarch64) ARCH="arm64" ;;
  i386|i686) ARCH="i386" ;;
  *)
    error "Unsupported architecture: $ARCH"
    ;;
esac

TAR_FILE="cozypkg-$OS-$ARCH.tar.gz"
CHECKSUM_FILE="cozypkg-checksums.txt"

if [ "$TAG" = "latest" ]; then
  BASE_URL="https://github.com/cozystack/cozypkg/releases/latest/download"
else
  BASE_URL="https://github.com/cozystack/cozypkg/releases/download/$TAG"
fi

TMPDIR=$(mktemp -d)
cleanup() { rm -rf "$TMPDIR"; }
trap cleanup EXIT INT TERM

info "Installing cozypkg version: $TAG"
info "Downloading $TAR_FILE..."
download "$TMPDIR/$TAR_FILE" "$BASE_URL/$TAR_FILE" || error "Failed to download $TAR_FILE"

info "Downloading checksums..."
download "$TMPDIR/$CHECKSUM_FILE" "$BASE_URL/$CHECKSUM_FILE" || error "Failed to download $CHECKSUM_FILE"

EXPECTED_SUM=$(grep "  $TAR_FILE" "$TMPDIR/$CHECKSUM_FILE" | awk '{print $1}')
[ -n "$EXPECTED_SUM" ] || error "Checksum not found for $TAR_FILE"

ACTUAL_SUM=$(sha256sum "$TMPDIR/$TAR_FILE" | awk '{print $1}')

if [ "$EXPECTED_SUM" != "$ACTUAL_SUM" ]; then
  error "Checksum verification failed!\nExpected: $EXPECTED_SUM\nActual:   $ACTUAL_SUM"
fi

success "Checksum verified."

info "Extracting..."
tar -xzf "$TMPDIR/$TAR_FILE" -C "$TMPDIR"

[ -f "$TMPDIR/cozypkg" ] || error "Binary 'cozypkg' not found in archive."

chmod +x "$TMPDIR/cozypkg"

# Determine install dir
if [ "$(id -u)" = "0" ] || [ -w "/usr/local/bin" ]; then
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
info "Run 'cozypkg --help' to get started."
