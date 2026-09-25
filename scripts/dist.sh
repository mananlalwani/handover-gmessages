#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
cd "$ROOT"

VERSION="${HANDOVER_GMESSAGES_VERSION:-0.3.2}"
HOST="${GOARCH:-$(go env GOARCH)}"
OS="${GOOS:-$(go env GOOS)}"
STAGE="handover-gmessages-${VERSION}-${OS}-${HOST}"
OUT_DIR="${ROOT}/dist"
STAGE_DIR="${OUT_DIR}/${STAGE}"

go build -o "${ROOT}/handover-gmessages" .

rm -rf "$STAGE_DIR"
mkdir -p "$STAGE_DIR/bin"
install -Dm755 "${ROOT}/handover-gmessages" "$STAGE_DIR/bin/handover-gmessages"
install -Dm644 LICENSE "$STAGE_DIR/LICENSE"
install -Dm755 "${ROOT}/scripts/install-from-dist.sh" "$STAGE_DIR/install.sh"

cat >"$STAGE_DIR/BUILD.txt" <<EOF
Handover Google Messages adapter ${VERSION}
GOOS=${OS} GOARCH=${HOST}
AGPL-3.0-only. Do not vendor this binary or libgm into the MIT Handover tree.

Install:
  tar xf ${STAGE}.tar.gz
  cd ${STAGE}
  ./install.sh

Then point Handover at ~/.local/bin/handover-gmessages:
  export HANDOVER_GMESSAGES_HELPER=\$HOME/.local/bin/handover-gmessages
EOF

mkdir -p "$OUT_DIR"
tar -C "$OUT_DIR" -czf "${OUT_DIR}/${STAGE}.tar.gz" "$STAGE"
rm -rf "$STAGE_DIR"
echo "${OUT_DIR}/${STAGE}.tar.gz"
