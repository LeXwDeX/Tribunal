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

die() {
  printf 'codebase-memory-mcp wrapper: %s\n' "$1" >&2
  exit 1
}

umask 077
CBM_CACHE_DIR="$HOME/.cache/codebase-memory-mcp"
CBM_RUNTIME_DIR="$HOME/run"

# codebase-memory-mcp 0.12.1 binds unix sockets (cbm-<id>.sock) inside
# CBM_RUNTIME_DIR, and a socket path is capped by sun_path (104 bytes on macOS,
# 108 on Linux). Once the runtime dir passes about 60 bytes, every call,
# including "cli index_repository", fails with "secure CLI coordination could
# not be created". Keep "$HOME/run" while it is at most 48 bytes; otherwise use
# a short private directory per HOME under /tmp. Only the coordination files
# move: the graph data stays in CBM_CACHE_DIR under HOME. The length is counted
# in bytes (C locale), because the limit is in bytes.
if [ "$(LC_ALL=C; printf '%s' "${#CBM_RUNTIME_DIR}")" -gt 48 ]; then
  uid=$(id -u)
  case "$uid" in
    '' | *[!0-9]*) die "unexpected output from id -u: $uid" ;;
  esac
  if command -v sha256sum >/dev/null 2>&1; then
    home_hash=$(printf '%s' "$HOME" | sha256sum | cut -c1-16)
  elif command -v shasum >/dev/null 2>&1; then
    home_hash=$(printf '%s' "$HOME" | shasum -a 256 | cut -c1-16)
  else
    die 'sha256sum or shasum is required to derive the runtime directory'
  fi
  if [ "${#home_hash}" -ne 16 ]; then
    die 'could not hash HOME for the runtime directory'
  fi
  case "$home_hash" in
    *[!0-9a-f]*) die 'could not hash HOME for the runtime directory' ;;
  esac

  runtime_parent="/tmp/tcbm-$uid"
  CBM_RUNTIME_DIR="$runtime_parent/$home_hash"
  # Plain mkdir (no -p) refuses any existing path, symlinks included, so
  # nothing planted in the shared /tmp is adopted: an existing parent must be
  # this user's own real directory (test -O is not POSIX, but dash, bash and
  # busybox sh all implement it).
  if ! mkdir -- "$runtime_parent" 2>/dev/null; then
    # shellcheck disable=SC3067
    if [ -L "$runtime_parent" ] || [ ! -d "$runtime_parent" ] || [ ! -O "$runtime_parent" ]; then
      die "$runtime_parent must be a real directory owned by the current user"
    fi
  fi
  # Deep HOMEs are per review (target commit graphs) and never come back, so
  # their coordination directories would pile up. Best effort: drop leaves of
  # other HOMEs that have not changed for two days. A live server keeps its own
  # leaf; review runs are far shorter than the cutoff.
  hex='[0-9a-f]'
  find "$runtime_parent" -mindepth 1 -maxdepth 1 -type d -mtime +2 \
    -name "$hex$hex$hex$hex$hex$hex$hex$hex$hex$hex$hex$hex$hex$hex$hex$hex" \
    ! -name "$home_hash" -exec rm -rf -- {} + 2>/dev/null || true
fi
export CBM_CACHE_DIR CBM_RUNTIME_DIR

mkdir -p -- "$CBM_CACHE_DIR" "$CBM_RUNTIME_DIR"

# Keep stdout clean for MCP stdio; set -e aborts on any setup failure.
for key in auto_index auto_watch watcher_enabled ui_enabled; do
  codebase-memory-mcp config set "$key" false >/dev/null
done

exec codebase-memory-mcp "$@"
