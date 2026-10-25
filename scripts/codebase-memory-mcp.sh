#!/bin/sh
# Run codebase-memory-mcp with cache and runtime confined to the provided HOME.
set -eu

case "${HOME:-}" in
  /*) ;;
  *)
    printf 'codebase-memory-mcp wrapper: HOME must be a nonempty absolute path\n' >&2
    exit 1
    ;;
esac

umask 077
CBM_CACHE_DIR="$HOME/.cache/codebase-memory-mcp"
CBM_RUNTIME_DIR="$HOME/run"
export CBM_CACHE_DIR CBM_RUNTIME_DIR

mkdir -p -- "$CBM_CACHE_DIR" "$CBM_RUNTIME_DIR"

# Keep stdout clean for MCP stdio; set -e aborts on any setup failure.
for key in auto_index auto_watch watcher_enabled ui_enabled; do
  codebase-memory-mcp config set "$key" false >/dev/null
done

exec codebase-memory-mcp "$@"
