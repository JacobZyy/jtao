#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

# Prefer the user-installed binary so `make install` updates take effect immediately
# without reinstalling the Codex plugin cache.
if [ -x "${HOME}/.local/bin/jtao" ]; then
  exec "${HOME}/.local/bin/jtao" "$@"
fi

if command -v jtao >/dev/null 2>&1; then
  exec "$(command -v jtao)" "$@"
fi

if [ -x "${SCRIPT_DIR}/jtao" ]; then
  exec "${SCRIPT_DIR}/jtao" "$@"
fi

echo "jtao binary not found in ~/.local/bin/jtao, PATH, or ${SCRIPT_DIR}/jtao" >&2
exit 127
