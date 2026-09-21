#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")" && pwd)
PREFIX="${PREFIX:-$HOME/.local}"

install -Dm755 "$ROOT/bin/handover-gmessages" "$PREFIX/bin/handover-gmessages"
echo "installed $PREFIX/bin/handover-gmessages"
echo "export HANDOVER_GMESSAGES_HELPER=$PREFIX/bin/handover-gmessages"
