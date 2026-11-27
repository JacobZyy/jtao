#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

for candidate in "${HOME}/.local/bin/jtao" /opt/homebrew/bin/jtao /usr/local/bin/jtao; do
  if [ -x "$candidate" ]; then
    exec "$candidate" "$@"
  fi
done

if command -v jtao >/dev/null 2>&1; then
  exec "$(command -v jtao)" "$@"
fi

if [ -x "${SCRIPT_DIR}/jtao" ]; then
  exec "${SCRIPT_DIR}/jtao" "$@"
fi

echo "jtao binary not found in ~/.local/bin/jtao, PATH, or ${SCRIPT_DIR}/jtao" >&2
exit 127
