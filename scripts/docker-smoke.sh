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
TMP_DIR=$(mktemp -d "${TMPDIR:-/tmp}/crc-smoke.XXXXXX")
PROJECT="crc-smoke-$(date +%s)-$$"
VOLUME_NAME="${PROJECT}_data"
SMOKE_IMAGE="crc-review:smoke-$PROJECT"
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

# Shell exports take precedence over --env-file and the project .env. Setting
# them here also overrides production variables the caller already exported.
export CRC_HTTP_PORT=0
export CRC_PUBLIC_URL="http://127.0.0.1:8080"
export GITLAB_URL="https://gitlab.invalid"
export REVIEW_MASTER_KEY="MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="
export REVIEW_ADMIN_TOKEN="smoke-admin-token-0123456789abcdef"
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
    # Exact tag only: the base image crc-review:local belongs to the user.
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

step "checking reviewctl"
projects=$(compose exec -T review /app/reviewctl \
  --url http://127.0.0.1:8080 projects list 2>>"$LOG") \
  || fail "reviewctl projects list failed"
compact=$(printf '%s' "$projects" | tr -d '[:space:]')
[ "$compact" = '{"projects":[]}' ] \
  || fail "reviewctl projects list did not return an empty project list"

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
  sh -eu -c 'printf "%s" "crc-smoke-marker" > /app/data/smoke-marker'

step "recreating $PROJECT and checking volume persistence"
compose up -d --force-recreate --wait --wait-timeout 180 >>"$LOG" 2>&1 \
  || fail "recreated service was not healthy within 180s"
marker=$(compose exec -T review cat /app/data/smoke-marker 2>>"$LOG") \
  || fail "cannot read the persisted marker"
[ "$marker" = "crc-smoke-marker" ] || fail "persisted marker changed after recreation"
assert_exec "database did not persist across recreation" \
  sh -eu -c 'test -f /app/data/review.db'

step "smoke checks passed"
