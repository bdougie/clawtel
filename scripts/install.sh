#!/usr/bin/env bash
# Install clawtel - local token telemetry for claw.tech
# Usage: curl -fsSL https://raw.githubusercontent.com/bdougie/clawtel/main/scripts/install.sh | bash
set -euo pipefail

REPO="bdougie/clawtel"
INSTALL_DIR="${CLAWTEL_INSTALL_DIR:-/usr/local/bin}"

# Detect OS
OS="$(uname -s)"
case "$OS" in
  Linux)  OS="linux" ;;
  Darwin) OS="darwin" ;;
  *)      echo "Unsupported OS: $OS" >&2; exit 1 ;;
esac

# Detect architecture
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64)  ARCH="amd64" ;;
  arm64|aarch64)  ARCH="arm64" ;;
  *)              echo "Unsupported architecture: $ARCH" >&2; exit 1 ;;
esac

ASSET="clawtel_${OS}_${ARCH}.tar.gz"
URL="https://github.com/${REPO}/releases/latest/download/${ASSET}"

echo "clawtel: installing ${OS}/${ARCH} from ${REPO}"
echo "clawtel: downloading ${ASSET}"

TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

curl -fsSL "$URL" -o "${TMPDIR}/${ASSET}"
tar xzf "${TMPDIR}/${ASSET}" -C "$TMPDIR"

if [ -w "$INSTALL_DIR" ]; then
  mv "${TMPDIR}/clawtel" "${INSTALL_DIR}/clawtel"
else
  echo "clawtel: installing to ${INSTALL_DIR} (requires sudo)"
  sudo mv "${TMPDIR}/clawtel" "${INSTALL_DIR}/clawtel"
fi

chmod +x "${INSTALL_DIR}/clawtel"

echo "clawtel: installed to ${INSTALL_DIR}/clawtel"
echo "clawtel: $(${INSTALL_DIR}/clawtel --version 2>&1 || echo 'v0.1.0')"
echo ""
echo "Set these env vars to enable telemetry:"
echo "  export CLAW_ID=\"your-claw-name\""
echo "  export CLAW_INGEST_KEY=\"ik_your_key_here\""
