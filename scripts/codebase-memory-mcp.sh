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

# A runtime directory under /tmp outlives its HOME, and review target graphs
# (one HOME per review, deleted when the review ends) never come back, so their
# directories would pile up. Each directory records the HOME it serves in
# ".home"; one whose HOME no longer exists cannot be used again. Both functions
# are best effort: a failure is ignored and nothing is written to stdout.
nl='
'

record_runtime_owner() {
  case "$HOME" in
    *"$nl"*) return 0 ;; # a record is one line; such a directory is never pruned
  esac
  record_tmp="$CBM_RUNTIME_DIR/.home.$$"
  # Write beside the record and rename, so a reader never sees a partial one.
  if { printf '%s\n' "$HOME" >"$record_tmp"; } 2>/dev/null &&
    mv -f -- "$record_tmp" "$CBM_RUNTIME_DIR/.home" 2>/dev/null; then
    return 0
  fi
  rm -f -- "$record_tmp" 2>/dev/null || true
  return 1
}

# Remove the other directories under runtime_parent whose recorded HOME is gone.
# Only a directory named like the ones this wrapper creates (16 lowercase hex
# digits, a real directory, not the current one) with a trustworthy record is
# considered: a regular, non-symlink ".home" of at most 4096 bytes holding one
# absolute path. Anything else, including a directory without a record (a
# wrapper that has not written it yet, or an older one), is left alone.
prune_stale_runtime_dirs() {
  for leaf in "$runtime_parent"/*; do
    leaf_name=${leaf##*/}
    case "$leaf_name" in
      "$home_hash") continue ;;
      *[!0-9a-f]*) continue ;;
    esac
    if [ "${#leaf_name}" -ne 16 ] || [ -L "$leaf" ] || [ ! -d "$leaf" ]; then
      continue
    fi
    owner_file="$leaf/.home"
    if [ -L "$owner_file" ] || [ ! -f "$owner_file" ] ||
      [ -z "$(find "$owner_file" -size -4097c -print 2>/dev/null)" ]; then
      continue
    fi
    owner_home=
    # Exactly one complete line.
    if ! { IFS= read -r owner_home && ! IFS= read -r _; } 2>/dev/null <"$owner_file"; then
      continue
    fi
    case "$owner_home" in
      /*) ;;
      *) continue ;;
    esac
    # Only a HOME that is provably gone frees its leaf: its parent must still
    # exist, so a failed stat (an unmounted volume, a parent made unreadable)
    # never counts as removal.
    if [ ! -e "$owner_home" ] && [ ! -L "$owner_home" ] && [ -d "$(dirname -- "$owner_home")" ]; then
      rm -rf -- "$leaf" 2>/dev/null || true
    fi
  done
}

umask 077
CBM_CACHE_DIR="$HOME/.cache/codebase-memory-mcp"
CBM_RUNTIME_DIR="$HOME/run"
runtime_parent=

# codebase-memory-mcp 0.12.1 binds unix sockets (cbm-<id>.sock) inside
# CBM_RUNTIME_DIR, and a socket path is capped by sun_path (104 bytes on macOS,
# 108 on Linux). Once the runtime dir passes about 60 bytes, every call,
# including "cli index_repository", fails with "secure CLI coordination could
# not be created" (measured on macOS: 60 bytes work, 70 fail). Keep "$HOME/run"
# while it is at most 57 bytes, which covers the standard Docker layout
# (/app/data/workspaces/<id>/.mcp/codebase-memory/home/run is 52 to 57 bytes for
# project ids up to 6 digits); otherwise use a short private directory per HOME
# under /tmp. Only the coordination files move: the graph data stays in
# CBM_CACHE_DIR under HOME. The length is counted in bytes (C locale), because
# the limit is in bytes.
if [ "$(LC_ALL=C; printf '%s' "${#CBM_RUNTIME_DIR}")" -gt 57 ]; then
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
  # An own parent that already exists keeps the mode it was created with, so
  # normalize it: nobody else may add to it or list it.
  chmod 700 "$runtime_parent" 2>/dev/null || die "$runtime_parent could not be made private"
fi
export CBM_CACHE_DIR CBM_RUNTIME_DIR

mkdir -p -- "$CBM_CACHE_DIR" "$CBM_RUNTIME_DIR"

if [ -n "$runtime_parent" ]; then
  record_runtime_owner || true
  prune_stale_runtime_dirs || true
fi

# Keep stdout clean for MCP stdio; set -e aborts on any setup failure.
for key in auto_index auto_watch watcher_enabled ui_enabled; do
  codebase-memory-mcp config set "$key" false >/dev/null
done

exec codebase-memory-mcp "$@"
