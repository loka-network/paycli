#!/usr/bin/env sh
#
# lokapay one-shot installer.
#
#   curl -fsSL https://github.com/loka-network/paycli/releases/latest/download/install.sh | sh
#
# Env overrides:
#   LOKAPAY_VERSION       release tag to install (default: latest)
#   LOKAPAY_INSTALL_DIR   target dir (default: $HOME/.local/bin, or /usr/local/bin if running as root)
#
# Detects OS/arch and downloads the matching tarball/zip from the
# GitHub release. Extracts just the `lokapay` binary, chmods it, and
# prints a PATH hint. Idempotent — re-runs replace the existing binary.

set -eu

REPO="loka-network/paycli"
BIN="lokapay"
VERSION="${LOKAPAY_VERSION:-latest}"

# ---- platform detection ----

uname_s=$(uname -s)
uname_m=$(uname -m)

case "$uname_s" in
    Darwin)             OS="darwin" ;;
    Linux)              OS="linux" ;;
    MINGW*|MSYS*|CYGWIN*) OS="windows" ;;
    *)
        echo "error: unsupported OS '$uname_s' — install via 'go install $REPO/cmd/lokapay@latest'" >&2
        exit 1
        ;;
esac

case "$uname_m" in
    x86_64|amd64)       ARCH="amd64" ;;
    aarch64|arm64)      ARCH="arm64" ;;
    *)
        echo "error: unsupported arch '$uname_m' — install via 'go install $REPO/cmd/lokapay@latest'" >&2
        exit 1
        ;;
esac

# Upstream loka-lnd doesn't ship windows/arm64, and goreleaser's matrix
# skips it for parity. Fail early rather than 404 mid-download.
if [ "$OS" = "windows" ] && [ "$ARCH" = "arm64" ]; then
    echo "error: lokapay does not publish a windows/arm64 release yet — install via 'go install'." >&2
    exit 1
fi

# ---- install dir ----

if [ -z "${LOKAPAY_INSTALL_DIR:-}" ]; then
    if [ "$(id -u 2>/dev/null || echo 1000)" -eq 0 ]; then
        LOKAPAY_INSTALL_DIR="/usr/local/bin"
    else
        LOKAPAY_INSTALL_DIR="$HOME/.local/bin"
    fi
fi

# ---- resolve version ----

if [ "$VERSION" = "latest" ]; then
    # The /releases/latest endpoint 302-redirects to /releases/tag/<TAG>;
    # follow once with curl -I and parse Location. Avoids the JSON API
    # rate limit for unauthenticated callers.
    TAG=$(curl -fsSLI -o /dev/null -w '%{url_effective}\n' "https://github.com/$REPO/releases/latest" \
        | sed -E 's|.*/tag/([^/]+).*|\1|')
    if [ -z "$TAG" ] || [ "$TAG" = "https://github.com/$REPO/releases/latest" ]; then
        echo "error: could not resolve latest release tag from github.com/$REPO" >&2
        exit 1
    fi
else
    TAG="$VERSION"
fi

VERSION_NO_V="${TAG#v}"

# ---- archive name ----

case "$OS" in
    windows) EXT="zip";    BIN_NAME="${BIN}.exe" ;;
    *)       EXT="tar.gz"; BIN_NAME="${BIN}" ;;
esac
ARCHIVE="${BIN}_${VERSION_NO_V}_${OS}_${ARCH}.${EXT}"
URL="https://github.com/$REPO/releases/download/$TAG/$ARCHIVE"

# ---- download + extract ----

TMP=$(mktemp -d 2>/dev/null || mktemp -d -t lokapay-XXXXXX)
trap 'rm -rf "$TMP"' EXIT INT TERM

echo "→ downloading $URL"
if ! curl -fsSL "$URL" -o "$TMP/$ARCHIVE"; then
    echo "error: download failed. Check that $TAG exists at github.com/$REPO/releases." >&2
    exit 1
fi

echo "→ extracting"
case "$ARCHIVE" in
    *.tar.gz) tar -xzf "$TMP/$ARCHIVE" -C "$TMP" ;;
    *.zip)
        if command -v unzip >/dev/null 2>&1; then
            unzip -q "$TMP/$ARCHIVE" -d "$TMP"
        else
            echo "error: unzip not found — install it or use the tar.gz on a unix host." >&2
            exit 1
        fi
        ;;
esac

if [ ! -f "$TMP/$BIN_NAME" ]; then
    echo "error: archive $ARCHIVE did not contain $BIN_NAME" >&2
    exit 1
fi

mkdir -p "$LOKAPAY_INSTALL_DIR"
mv "$TMP/$BIN_NAME" "$LOKAPAY_INSTALL_DIR/$BIN_NAME"
chmod +x "$LOKAPAY_INSTALL_DIR/$BIN_NAME"

echo "✓ installed $BIN $TAG to $LOKAPAY_INSTALL_DIR/$BIN_NAME"

# ---- PATH check ----

case ":$PATH:" in
    *":$LOKAPAY_INSTALL_DIR:"*)  on_path=1 ;;
    *)                            on_path=0 ;;
esac

if [ "$on_path" = "0" ]; then
    echo
    echo "$LOKAPAY_INSTALL_DIR is not on your PATH. Add it:"
    echo "  export PATH=\"$LOKAPAY_INSTALL_DIR:\$PATH\""
fi

echo
echo "Next:"
echo "  $BIN init"
