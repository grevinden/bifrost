#!/usr/bin/env bash
set -euo pipefail

# Build script for bifrost-http .deb package (full UI included)
# Usage: ./packaging/deb/build-deb.sh [VERSION]
#   If no VERSION is given, reads from git tag or falls back to "1.0.0"
#
# Requires: Go 1.26+, Node.js 20+, npm

ROOT_DIR="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT_DIR"

BINARY="tmp/bifrost-http"
PACKAGE_NAME="bifrost-http"

# ── Version ──────────────────────────────────────────────────────────
if [ $# -ge 1 ] && [ -n "$1" ]; then
    VERSION="$1"
else
    VERSION="$(git describe --tags --abbrev=0 2>/dev/null || echo "v0.0.0")"
fi
VERSION="${VERSION#v}"
echo "Building bifrost-http .deb — version ${VERSION}"

# ── Build UI ────────────────────────────────────────────────────────
echo "→ Building UI (React + Vite)..."
if ! command -v node &>/dev/null; then
    echo "❌ Node.js is required to build the UI. Install Node.js 20+ first."
    exit 1
fi
if ! command -v npm &>/dev/null; then
    echo "❌ npm is required to build the UI. Install npm first."
    exit 1
fi
echo "   Node.js $(node -v) — npm $(npm -v)"

cd ui
npm ci
npm run build
cd "$ROOT_DIR"

echo "→ UI built successfully"

# ── Build Go binary (with embedded UI) ──────────────────────────────
echo "→ Building Go binary..."
mkdir -p tmp

cd transports/bifrost-http && CGO_ENABLED=1 GOWORK=off go build \
    -ldflags="-w -s -X main.Version=v${VERSION}" \
    -a -trimpath \
    -tags "sqlite_dynamic" \
    -o "../../${BINARY}" .
cd "$ROOT_DIR"

echo "→ Binary built: ${BINARY} ($(ls -lh "${BINARY}" | awk '{print $5}'))"

# ── Prepare .deb package directory ──────────────────────────────────
DEB_DIR="$(mktemp -d)"
trap 'rm -rf "$DEB_DIR"' EXIT

# Copy DEBIAN control files
cp -r packaging/deb/DEBIAN "$DEB_DIR/"

# Substitute version in control file
sed -i "s/__VERSION__/${VERSION}/g" "$DEB_DIR/DEBIAN/control"

# Make maintainer scripts executable
chmod 755 "$DEB_DIR/DEBIAN/postinst"
chmod 755 "$DEB_DIR/DEBIAN/prerm"
chmod 755 "$DEB_DIR/DEBIAN/postrm"

# Copy binary
mkdir -p "$DEB_DIR/usr/local/bin"
cp "${BINARY}" "$DEB_DIR/usr/local/bin/bifrost-http"
chmod 755 "$DEB_DIR/usr/local/bin/bifrost-http"

# Copy config (example as default, user must edit)
mkdir -p "$DEB_DIR/etc/bifrost"
cp packaging/deb/etc/bifrost/config.json.example "$DEB_DIR/etc/bifrost/config.json"

# Copy systemd service
mkdir -p "$DEB_DIR/lib/systemd/system"
cp packaging/deb/lib/systemd/system/bifrost.service "$DEB_DIR/lib/systemd/system/"

# ── Build .deb ──────────────────────────────────────────────────────
OUTPUT_DIR="${ROOT_DIR}/tmp"
mkdir -p "$OUTPUT_DIR"
DEB_FILE="${OUTPUT_DIR}/${PACKAGE_NAME}_${VERSION}_amd64.deb"

if command -v fakeroot &>/dev/null; then
    fakeroot dpkg-deb --build "$DEB_DIR" "$DEB_FILE"
else
    echo "⚠  fakeroot not found — try running as root or install fakeroot"
    dpkg-deb --build "$DEB_DIR" "$DEB_FILE"
fi

echo ""
echo "✅ .deb package created: ${DEB_FILE}"
echo "   Size: $(ls -lh "${DEB_FILE}" | awk '{print $5}')"
echo ""
echo "Install on target server:"
echo "  sudo dpkg -i ${DEB_FILE}"
echo "  sudo systemctl edit bifrost.service   # set API keys in environment"
echo "  sudo systemctl start bifrost.service"
echo ""
