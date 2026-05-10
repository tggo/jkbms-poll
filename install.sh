#!/bin/sh
# jkbms-poll quick installer.
#
# Detects OS/arch, downloads the matching binary from the latest GitHub
# release, verifies its SHA-256, and installs it.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/tggo/jkbms-poll/main/install.sh | sh
#   curl -fsSL https://raw.githubusercontent.com/tggo/jkbms-poll/main/install.sh | sh -s -- v0.1.0
#   curl -fsSL https://raw.githubusercontent.com/tggo/jkbms-poll/main/install.sh | INSTALL_DIR=$HOME/bin sh
#
# Env:
#   VERSION       (or first arg)  e.g. v0.1.0; default: latest
#   INSTALL_DIR                   default: /usr/local/bin (sudo if not writable)
set -eu

REPO="tggo/jkbms-poll"
VERSION="${1:-${VERSION:-latest}}"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
BIN_NAME="jkbms-poll"

err() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }

os=$(uname -s | tr '[:upper:]' '[:lower:]')
[ "$os" = linux ] || err "unsupported OS: $os (this binary is Linux-only — BLE backend uses BlueZ)"

uname_m=$(uname -m)
case "$uname_m" in
  x86_64|amd64)             arch=amd64 ;;
  aarch64|arm64)            arch=arm64 ;;
  armv7l|armv7|armhf)       arch=armv7 ;;
  armv6l|armv6)             arch=armv6 ;;
  *) err "unsupported arch: $uname_m" ;;
esac

asset="${BIN_NAME}-${os}-${arch}"

if [ "$VERSION" = latest ]; then
  url_base="https://github.com/${REPO}/releases/latest/download"
else
  url_base="https://github.com/${REPO}/releases/download/${VERSION}"
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "→ downloading $asset (${VERSION})"
curl -fsSL "${url_base}/${asset}"     -o "$tmp/$asset"
curl -fsSL "${url_base}/SHA256SUMS"   -o "$tmp/SHA256SUMS"

echo "→ verifying SHA-256"
expected=$(grep " ${asset}\$" "$tmp/SHA256SUMS" | awk '{print $1}')
[ -n "$expected" ] || err "no checksum line for $asset in SHA256SUMS"
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$tmp/$asset" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
  actual=$(shasum -a 256 "$tmp/$asset" | awk '{print $1}')
else
  err "need sha256sum or shasum to verify download"
fi
[ "$actual" = "$expected" ] || err "checksum mismatch: got $actual, want $expected"

chmod +x "$tmp/$asset"

if [ -w "$INSTALL_DIR" ]; then
  mv "$tmp/$asset" "$INSTALL_DIR/$BIN_NAME"
else
  echo "→ $INSTALL_DIR not writable, using sudo"
  sudo mv "$tmp/$asset" "$INSTALL_DIR/$BIN_NAME"
fi

echo "✓ installed $INSTALL_DIR/$BIN_NAME"
"$INSTALL_DIR/$BIN_NAME" -h 2>&1 | head -3 || true
