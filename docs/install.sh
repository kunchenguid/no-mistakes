#!/bin/sh
set -e

REPO="kunchenguid/no-mistakes"

case ":$PATH:" in
  *":$HOME/.local/bin:"*) INSTALL_DIR="$HOME/.local/bin" ;;
  *) INSTALL_DIR="/usr/local/bin" ;;
esac

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"

case "$OS" in
  darwin|linux) ;;
  *) echo "Unsupported OS: $OS"; exit 1 ;;
esac

case "$ARCH" in
  x86_64|amd64) ARCH="amd64" ;;
  arm64|aarch64) ARCH="arm64" ;;
  *) echo "Unsupported architecture: $ARCH"; exit 1 ;;
esac

VERSION="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)"
if [ -z "$VERSION" ]; then
  echo "Could not determine latest release"
  exit 1
fi

FILENAME="no-mistakes-${VERSION}-${OS}-${ARCH}.tar.gz"
URL="https://github.com/${REPO}/releases/download/${VERSION}/${FILENAME}"

TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

echo "Downloading no-mistakes ${VERSION} for ${OS}/${ARCH}..."
curl -fsSL "$URL" -o "${TMPDIR}/${FILENAME}"
tar xzf "${TMPDIR}/${FILENAME}" -C "$TMPDIR"

mkdir -p "$INSTALL_DIR" 2>/dev/null || true

if [ -w "$INSTALL_DIR" ]; then
  mv "${TMPDIR}/no-mistakes" "${INSTALL_DIR}/no-mistakes"
else
  echo "Installing to ${INSTALL_DIR} (requires sudo)..."
  sudo mkdir -p "$INSTALL_DIR"
  sudo mv "${TMPDIR}/no-mistakes" "${INSTALL_DIR}/no-mistakes"
fi

chmod 755 "${INSTALL_DIR}/no-mistakes" 2>/dev/null || true
echo "no-mistakes ${VERSION} installed to ${INSTALL_DIR}/no-mistakes"
