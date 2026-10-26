#!/usr/bin/env bash
# Bounded Docker Compose smoke test for the container deployment contract.
#
# Safe for local use: the repository .env is never read or written. The script
# forces fake environment values, runs in a unique Compose project with a
# unique volume and image tag, and removes only those resources and its own
# temporary directory. CI supplies the outer timeout; --wait-timeout bounds
# container startup and recreation here.
set -euo pipefail

fail() {
  printf 'docker-smoke: %s\n' "$*" >&2
  exit 1
}

command -v docker >/dev/null 2>&1 || fail "Docker CLI is required"
docker compose version >/dev/null 2>&1 || fail "Docker Compose v2 is required (docker compose)"
docker info >/dev/null 2>&1 || fail "Docker daemon is not reachable"

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)
REPO_ROOT=$(CDPATH='' cd -- "$SCRIPT_DIR/.." && pwd -P)
COMPOSE_FILE="$REPO_ROOT/compose.yaml"
CONFIG_FILE="$REPO_ROOT/config/server.docker.yaml"
[ -f "$COMPOSE_FILE" ] || fail "missing $COMPOSE_FILE"
[ -f "$CONFIG_FILE" ] || fail "missing $CONFIG_FILE"

umask 077
TMP_DIR=$(mktemp -d "${TMPDIR:-/tmp}/tribunal-smoke.XXXXXX")
PROJECT="tribunal-smoke-$(date +%s)-$$"
VOLUME_NAME="${PROJECT}_data"
SMOKE_IMAGE="tribunal:smoke-$PROJECT"
FAKE_ENV="$TMP_DIR/smoke.env"
OVERRIDE_FILE="$TMP_DIR/override.yaml"
LOG="$TMP_DIR/smoke.log"
IMAGE_BUILT=0
: >"$LOG"

# An explicit empty env file prevents loading the user's .env; exports below
# supply all interpolation values without keeping a second copy here.
: >"$FAKE_ENV"

cat >"$OVERRIDE_FILE" <<EOF
services:
  review:
    image: $SMOKE_IMAGE
volumes:
  data:
    name: $VOLUME_NAME
EOF

# Shell exports take precedence over --env-file and the project .env. Override
# both the new and legacy names so a caller's exported values or secrets never
# reach Compose interpolation.
export TRIBUNAL_HTTP_PORT=0
export CRC_HTTP_PORT=0
export TRIBUNAL_PUBLIC_URL="http://127.0.0.1:8080"
export CRC_PUBLIC_URL="http://127.0.0.1:8080"
export GITLAB_URL="https://gitlab.invalid"
export REVIEW_MASTER_KEY="MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="
export REVIEW_ADMIN_TOKEN="smoke-admin-token-0123456789abcdef"
export TRIBUNAL_SESSION_KEY="MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm"
export CRC_SESSION_KEY="MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm"
export LLM_PROTOCOL="openai-chat"
export LLM_BASE_URL="https://llm.invalid/v1"
export LLM_MODEL="smoke-model"
export LLM_API_KEY="smoke-not-a-real-key"
export GITLAB_OAUTH_CLIENT_ID=""
export GITLAB_OAUTH_CLIENT_SECRET=""
export GITLAB_ADMIN_USERNAME=""

compose() {
  docker compose \
    --project-name "$PROJECT" \
    --project-directory "$REPO_ROOT" \
    --env-file "$FAKE_ENV" \
    --file "$COMPOSE_FILE" \
    --file "$OVERRIDE_FILE" \
    "$@"
}

cleanup() {
  local status=$?
  set +e
  trap - EXIT INT TERM
  if [ "$status" -ne 0 ] && [ "${PROJECT_STARTED:-0}" = 1 ]; then
    compose logs --no-color --tail=80 >>"$LOG" 2>&1
  fi
  if [ "$status" -ne 0 ] && [ -s "$LOG" ]; then
    printf 'docker-smoke: last log lines:\n' >&2
    tail -n 80 "$LOG" >&2
  fi
  if [ "${PROJECT_STARTED:-0}" = 1 ]; then
    if ! compose down --volumes --remove-orphans >>"$LOG" 2>&1; then
      printf 'docker-smoke: warning: could not remove project %s (volume %s)\n' \
        "$PROJECT" "$VOLUME_NAME" >&2
      status=1
    fi
    # Exact tag only: the base image tribunal:local belongs to the user.
    if [ "$IMAGE_BUILT" = 1 ] && ! docker image rm "$SMOKE_IMAGE" >>"$LOG" 2>&1; then
      printf 'docker-smoke: warning: could not remove image %s\n' "$SMOKE_IMAGE" >&2
      status=1
    fi
  fi
  if ! rm -rf "$TMP_DIR"; then
    printf 'docker-smoke: warning: could not remove temporary directory %s\n' "$TMP_DIR" >&2
    status=1
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

step() {
  printf '==> %s\n' "$*"
}

assert_exec() {
  local label=$1
  shift
  if ! compose exec -T review "$@" >>"$LOG" 2>&1; then
    fail "$label"
  fi
}

http_status() {
  compose exec -T review curl --max-time 5 -sS -o /dev/null -w '%{http_code}' "$1" 2>>"$LOG"
}

# api sends one authenticated management request with the fake bootstrap
# token. The MCP test call starts a stdio server, so it gets a longer bound.
api() {
  compose exec -T review curl --max-time 90 -fsS -X "$1" \
    -H "Authorization: Bearer $REVIEW_ADMIN_TOKEN" "http://127.0.0.1:8080$2" 2>>"$LOG"
}

step "validating Compose configuration for $PROJECT"
compose config --quiet >>"$LOG" 2>&1 || fail "docker compose config rejected the deployment files"

# From here on the script may have created its own project resources; cleanup
# runs down --volumes only for this unique project and volume.
PROJECT_STARTED=1

step "building $SMOKE_IMAGE"
compose build review >>"$LOG" 2>&1 || fail "docker compose build failed"
IMAGE_BUILT=1

step "checking server configuration"
compose run --rm --no-deps -T review \
  --config /app/config/server.yaml --check-config >>"$LOG" 2>&1 \
  || fail "server --check-config failed"

step "starting $PROJECT (bounded to 180s)"
compose up -d --wait --wait-timeout 180 >>"$LOG" 2>&1 \
  || fail "docker compose up did not become healthy within 180s"

step "checking health and embedded console"
status=$(http_status http://127.0.0.1:8080/healthz) || fail "healthz request failed"
[ "$status" = 200 ] || fail "healthz status is $status, want 200"
status=$(http_status http://127.0.0.1:8080/readyz) || fail "readyz request failed"
[ "$status" = 200 ] || fail "readyz status is $status, want 200"
console=$(compose exec -T review curl --max-time 5 -fsS http://127.0.0.1:8080/ 2>>"$LOG") \
  || fail "root page request failed"
case "$console" in
  *'id="root"'*) ;;
  *) fail "root page does not serve the embedded console" ;;
esac

step "checking authentication defaults"
status=$(http_status http://127.0.0.1:8080/api/v1/projects) \
  || fail "anonymous management request failed"
[ "$status" = 401 ] || fail "anonymous management status is $status, want 401"
status=$(http_status http://127.0.0.1:8080/auth/login) || fail "OAuth login request failed"
[ "$status" = 503 ] || fail "OAuth login status is $status, want 503"

step "checking tribunalctl and compatibility path"
projects=$(compose exec -T review /app/tribunalctl \
  --url http://127.0.0.1:8080 projects list 2>>"$LOG") \
  || fail "tribunalctl projects list failed"
compact=$(printf '%s' "$projects" | tr -d '[:space:]')
[ "$compact" = '{"projects":[]}' ] \
  || fail "tribunalctl projects list did not return an empty project list"
legacy_projects=$(compose exec -T review /app/reviewctl \
  --url http://127.0.0.1:8080 projects list 2>>"$LOG") \
  || fail "compatibility reviewctl path failed"
legacy_compact=$(printf '%s' "$legacy_projects" | tr -d '[:space:]')
[ "$legacy_compact" = '{"projects":[]}' ] \
  || fail "compatibility reviewctl path did not return an empty project list"

step "checking runtime contract"
# shellcheck disable=SC2016 # commands expand inside the container
assert_exec "container uid is not 10001" sh -eu -c '[ "$(id -u)" = 10001 ]'
# shellcheck disable=SC2016
assert_exec "container gid is not 10001" sh -eu -c '[ "$(id -g)" = 10001 ]'
# shellcheck disable=SC2016
assert_exec "data directory is not 10001:10001 mode 700" \
  sh -eu -c '[ "$(stat -c "%u:%g %a" /app/data)" = "10001:10001 700" ]'
assert_exec "git is not installed" sh -eu -c 'command -v git >/dev/null 2>&1'
assert_exec "license texts are missing" \
  sh -eu -c 'test -s /licenses/LICENSE && test -s /licenses/THIRD_PARTY_NOTICES.txt'
assert_exec "database is missing" sh -eu -c 'test -f /app/data/review.db'
assert_exec "data directory is not writable" \
  sh -eu -c 'printf "%s" "tribunal-smoke-marker" > /app/data/smoke-marker'

step "checking bundled codebase-memory MCP"
# shellcheck disable=SC2016
assert_exec "codebase-memory-mcp 0.12.1 is not on PATH" \
  sh -eu -c '[ "$(codebase-memory-mcp --version)" = "codebase-memory-mcp 0.12.1" ]'
assert_exec "MCP wrapper is missing or writable by the service user" \
  sh -eu -c 'test -x /usr/local/bin/tribunal-codebase-memory-mcp && ! test -w /usr/local/bin/tribunal-codebase-memory-mcp'
# shellcheck disable=SC2016
assert_exec "crc-codebase-memory-mcp compatibility link is missing" \
  sh -eu -c '[ "$(readlink /usr/local/bin/crc-codebase-memory-mcp)" = tribunal-codebase-memory-mcp ]'
assert_exec "codebase-memory-mcp license texts are missing" \
  sh -eu -c 'test -s /licenses/codebase-memory-mcp/LICENSE && test -s /licenses/codebase-memory-mcp/THIRD_PARTY_NOTICES.md'
# The onboarding index_command form: the CLI indexes a fixed directory under
# an isolated HOME, and the index is visible only in that HOME. The files stay
# on the smoke volume, which cleanup removes.
if ! compose exec -T review sh -eu >>"$LOG" 2>&1 <<'EOF'
root=/app/data/cbm-smoke
mkdir -p "$root/home" "$root/other" "$root/base"
printf 'package a\n\nfunc Add(a, b int) int { return a + b }\n' >"$root/base/a.go"
HOME="$root/home" tribunal-codebase-memory-mcp cli --quiet index_repository "{\"repo_path\":\"$root/base\"}"
HOME="$root/home" tribunal-codebase-memory-mcp cli list_projects | grep -q cbm-smoke-base
! HOME="$root/other" tribunal-codebase-memory-mcp cli list_projects | grep -q cbm-smoke-base
EOF
then
  fail "codebase-memory CLI could not index an isolated repository"
fi
servers=$(api GET '/api/v1/mcp-servers?scope=platform') || fail "platform MCP server list request failed"
case "$servers" in
  *'"name":"codebase-memory"'*) ;;
  *) fail "default platform MCP server codebase-memory is not registered" ;;
esac
server_id=$(printf '%s' "$servers" | grep -o '"id":"[^"]*"' | head -n 1 | cut -d'"' -f4)
[ -n "$server_id" ] || fail "cannot read the codebase-memory server id"
tools=$(api POST "/api/v1/mcp-servers/$server_id/test") \
  || fail "codebase-memory MCP did not start and list tools"
for tool in search_graph trace_path get_code_snippet; do
  case "$tools" in
    *"\"name\":\"$tool\""*) ;;
    *) fail "codebase-memory MCP does not list $tool" ;;
  esac
done
for tool in index_repository delete_project ingest_traces manage_adr; do
  case "$tools" in
    *"\"name\":\"$tool\""*) fail "codebase-memory MCP exposes write tool $tool" ;;
  esac
done

step "recreating $PROJECT and checking volume persistence"
compose up -d --force-recreate --wait --wait-timeout 180 >>"$LOG" 2>&1 \
  || fail "recreated service was not healthy within 180s"
marker=$(compose exec -T review cat /app/data/smoke-marker 2>>"$LOG") \
  || fail "cannot read the persisted marker"
[ "$marker" = "tribunal-smoke-marker" ] || fail "persisted marker changed after recreation"
assert_exec "database did not persist across recreation" \
  sh -eu -c 'test -f /app/data/review.db'

step "smoke checks passed"
