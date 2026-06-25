#!/usr/bin/env bash
set -euo pipefail

# install-local.sh — Build no-mistakes from this repo and install it.
#
# Usage:  ./scripts/install-local.sh
#
# Installs to the directory that /usr/local/bin/no-mistakes symlinks to
# (default: ~/.no-mistakes/bin/no-mistakes). The existing binary is backed
# up as no-mistakes.bak in the same directory.

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
TARGET="$(readlink -f /usr/local/bin/no-mistakes 2>/dev/null || echo "$HOME/.no-mistakes/bin/no-mistakes")"
TARGET_DIR="$(dirname "$TARGET")"

VERSION="$(cd "$REPO_DIR" && git describe --tags --always --dirty 2>/dev/null || echo dev)"
COMMIT="$(cd "$REPO_DIR" && git rev-parse --short HEAD 2>/dev/null || echo unknown)"
DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

# back up existing binary
if [ -f "$TARGET" ]; then
  cp "$TARGET" "$TARGET_DIR/no-mistakes.bak"
  echo "backed up $TARGET -> $TARGET_DIR/no-mistakes.bak"
fi

mkdir -p "$TARGET_DIR"

cd "$REPO_DIR"
go build \
  -ldflags "-X github.com/kunchenguid/no-mistakes/internal/buildinfo.Version=$VERSION -X github.com/kunchenguid/no-mistakes/internal/buildinfo.Commit=$COMMIT -X github.com/kunchenguid/no-mistakes/internal/buildinfo.Date=$DATE" \
  -o "$TARGET" \
  ./cmd/no-mistakes

echo "installed no-mistakes ($VERSION, commit $COMMIT) to $TARGET"
