#!/usr/bin/env sh
# Install MySQL Tester on macOS or Linux.
#
#   curl -fsSL https://raw.githubusercontent.com/cubetiqlabs/cubisoft-tester/main/scripts/install.sh | sh
#
# Env: VERSION=1.2.3 to pin a version, INSTALL_DIR=... for the Linux target
# directory (default ~/.local/bin).
set -eu

REPO="cubetiqlabs/cubisoft-tester"
APP="mysqltester"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT INT TERM

case "$(uname -s)" in
  Darwin) OS=darwin ;;
  Linux)  OS=linux ;;
  *) echo "unsupported OS: $(uname -s). Windows users: see scripts/install.ps1" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  arm64|aarch64) ARCH=arm64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac
[ "$OS" = darwin ] && EXT=zip || EXT=tar.gz

VERSION=${VERSION:-}
if [ -z "$VERSION" ]; then
  VERSION=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" |
    sed -n 's/.*"tag_name" *: *"v\{0,1\}\([^"]*\)".*/\1/p' | head -1)
fi
[ -n "$VERSION" ] || { echo "could not determine the latest version" >&2; exit 1; }

ASSET="${APP}_${VERSION}_${OS}_${ARCH}.${EXT}"
BASE="https://github.com/$REPO/releases/download/v$VERSION"

echo "Downloading $ASSET"
curl -fsSL -o "$TMP/$ASSET" "$BASE/$ASSET"

# The release publishes checksums.txt; refuse to install a mismatched download.
if curl -fsSL -o "$TMP/checksums.txt" "$BASE/checksums.txt" 2>/dev/null; then
  WANT=$(grep " $ASSET\$" "$TMP/checksums.txt" | awk '{print $1}')
  if [ -n "$WANT" ]; then
    if command -v sha256sum >/dev/null 2>&1; then GOT=$(sha256sum "$TMP/$ASSET" | awk '{print $1}')
    else GOT=$(shasum -a 256 "$TMP/$ASSET" | awk '{print $1}'); fi
    [ "$WANT" = "$GOT" ] || { echo "checksum mismatch for $ASSET" >&2; exit 1; }
    echo "Checksum verified"
  fi
fi

if [ "$OS" = darwin ]; then
  unzip -q "$TMP/$ASSET" -d "$TMP/x"
  BUNDLE=$(find "$TMP/x" -maxdepth 1 -name '*.app' | head -1)
  [ -n "$BUNDLE" ] || { echo "no .app bundle in $ASSET" >&2; exit 1; }
  DEST="/Applications/$(basename "$BUNDLE")"
  rm -rf "$DEST"
  cp -R "$BUNDLE" "$DEST" 2>/dev/null || { sudo rm -rf "$DEST"; sudo cp -R "$BUNDLE" "$DEST"; }
  # The build is ad-hoc signed, so clear any quarantine flag Gatekeeper would trip on.
  xattr -dr com.apple.quarantine "$DEST" 2>/dev/null || true
  echo "Installed $DEST"
  echo "Open it with: open -a \"$(basename "$BUNDLE" .app)\""
else
  DIR=${INSTALL_DIR:-$HOME/.local/bin}
  mkdir -p "$DIR"
  tar -xzf "$TMP/$ASSET" -C "$TMP"
  install -m 0755 "$TMP/$APP" "$DIR/$APP"
  echo "Installed $DIR/$APP"
  case ":$PATH:" in
    *":$DIR:"*) echo "Run it with: $APP" ;;
    *) echo "Add $DIR to your PATH, or run it with: $DIR/$APP" ;;
  esac
fi
