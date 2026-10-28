#!/usr/bin/env bash
# Bounded Docker Compose smoke test for the container deployment contract.
#
# The repository .env is never read or written. A daemon-allocated volume
# supplies the project identity; cleanup removes only verified resources.
# CI supplies the outer timeout; --wait-timeout bounds container startup.
set -euo pipefail

TMP_DIR=''
TMP_CREATE_PENDING=0
LOG=''
PROJECT=''
VOLUME_NAME=''
VOLUME_CREATED=''
RUN_ID=''
SMOKE_IMAGE=''
IMAGE_ID=''
VOLUME_CONFIRMED=0
VOLUME_CREATE_PENDING=0
BUILD_STARTED=0
RUN_STARTED=0
MUTATION_ACTIVE=0
CAPTURED=0
KNOWN_CONTAINERS=''
KNOWN_NETWORKS=''

fail() {
  local status=$?
  printf 'docker-smoke: %s\n' "$*" >&2
  [ "$status" -ne 0 ] || status=1
  exit "$status"
}

cleanup() {
  local status=$?
  set +e
  trap - EXIT INT TERM
  if [ "$status" -ne 0 ] && [ -f "$LOG" ] && [ -s "$LOG" ]; then
    printf 'docker-smoke: last log lines:\n' >&2
    tail -n 80 "$LOG" >&2
  fi
  if [ "$RUN_STARTED" = 1 ]; then
    project_resources container remove || { [ "$status" -ne 0 ] || status=1; }
    project_resources network remove || { [ "$status" -ne 0 ] || status=1; }
  fi
  if [ "$VOLUME_CONFIRMED" = 1 ]; then
    if ! verify_volume; then
      printf 'docker-smoke: warning: volume %s has unknown ownership; retained\n' "$VOLUME_NAME" >&2
      [ "$status" -ne 0 ] || status=1
    elif ! docker volume rm "$VOLUME_NAME" >>"$LOG" 2>&1; then
      printf 'docker-smoke: warning: could not remove verified volume %s\n' "$VOLUME_NAME" >&2
      [ "$status" -ne 0 ] || status=1
    fi
  elif [ -n "$VOLUME_NAME" ]; then
    printf 'docker-smoke: warning: volume %s was not confirmed; retained\n' "$VOLUME_NAME" >&2
    [ "$status" -ne 0 ] || status=1
  elif [ "$VOLUME_CREATE_PENDING" = 1 ]; then
    printf 'docker-smoke: warning: volume creation outcome is unknown; any volume with run label %s is retained\n' "$RUN_ID" >&2
    [ "$status" -ne 0 ] || status=1
  fi
  if [ "$BUILD_STARTED" = 1 ]; then
    image_resource remove || { [ "$status" -ne 0 ] || status=1; }
  fi
  if [ -n "$TMP_DIR" ]; then
    if ! rm -rf -- "$TMP_DIR"; then
      printf 'docker-smoke: warning: could not remove temporary directory %s\n' "$TMP_DIR" >&2
      [ "$status" -ne 0 ] || status=1
    fi
  elif [ "$TMP_CREATE_PENDING" = 1 ]; then
    printf 'docker-smoke: warning: temporary directory creation outcome is unknown; any unreported directory is retained\n' >&2
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)
REPO_ROOT=$(CDPATH='' cd -- "$SCRIPT_DIR/.." && pwd -P)
COMPOSE_FILE="$REPO_ROOT/compose.yaml"
CONFIG_FILE="$REPO_ROOT/config/server.docker.yaml"
[ -f "$COMPOSE_FILE" ] || fail "missing $COMPOSE_FILE"
[ -f "$CONFIG_FILE" ] || fail "missing $CONFIG_FILE"

umask 077
TMP_CREATE_PENDING=1
TMP_DIR=$(mktemp -d "${TMPDIR:-/tmp}/tribunal-smoke.XXXXXX")
[ -n "$TMP_DIR" ] || fail "mktemp returned an empty directory path"
TMP_CREATE_PENDING=0
FAKE_ENV="$TMP_DIR/smoke.env"
OVERRIDE_FILE="$TMP_DIR/override.yaml"
LOG="$TMP_DIR/smoke.log"
: >"$LOG"

command -v docker >/dev/null 2>&1 || fail "Docker CLI is required"
docker compose version >/dev/null 2>&1 || fail "Docker Compose v2 is required (docker compose)"
docker info >/dev/null 2>&1 || fail "Docker daemon is not reachable"

compose() {
  docker compose \
    --project-name "$PROJECT" \
    --project-directory "$REPO_ROOT" \
    --env-file "$FAKE_ENV" \
    --file "$COMPOSE_FILE" \
    --file "$OVERRIDE_FILE" \
    "$@"
}

list_project_resources() {
  local kind=$1 by_name='' by_project='' result=0
  if [ "$kind" = container ]; then
    by_name=$(docker container ls --all --quiet --no-trunc --filter "name=$PROJECT" 2>>"$LOG") || result=1
    by_project=$(docker container ls --all --quiet --no-trunc --filter "label=com.docker.compose.project=$PROJECT" 2>>"$LOG") || result=1
  else
    by_name=$(docker network ls --quiet --no-trunc --filter "name=${PROJECT}_default" 2>>"$LOG") || result=1
    by_project=$(docker network ls --quiet --no-trunc --filter "label=com.docker.compose.project=$PROJECT" 2>>"$LOG") || result=1
  fi
  printf '%s\n%s\n' "$by_name" "$by_project" | sort -u || result=1
  return "$result"
}

list_images() {
  docker image ls --all --quiet --no-trunc --filter "reference=$SMOKE_IMAGE" 2>>"$LOG"
}

list_run_images() {
  docker image ls --all --quiet --no-trunc --filter "label=io.tribunal.smoke.run=$RUN_ID" 2>>"$LOG"
}

verify_volume() {
  local actual
  actual=$(docker volume inspect --format '{{.Name}}|{{index .Labels "io.tribunal.smoke.run"}}|{{.CreatedAt}}' "$VOLUME_NAME" 2>>"$LOG") || return 1
  [ "$actual" = "$VOLUME_NAME|$RUN_ID|$VOLUME_CREATED" ]
}

project_resources() {
  local kind=$1 action=$2 ids id actual known words result=0
  if [ "$kind" = container ]; then
    known=$KNOWN_CONTAINERS
  else
    known=$KNOWN_NETWORKS
  fi
  ids=$(list_project_resources "$kind") || {
    printf 'docker-smoke: warning: incomplete %s list; some resources may be retained\n' "$kind" >&2
    result=1
  }
  words=$(printf '%s\n' "$ids" | tr '\n' ' ')
  if [ "$CAPTURED" = 1 ] && [ "$MUTATION_ACTIVE" = 0 ]; then
    for id in $known; do
      case " $words " in
        *" $id "*) ;;
        *) printf 'docker-smoke: warning: known %s %s is missing or replaced\n' "$kind" "$id" >&2; result=1 ;;
      esac
    done
  fi
  for id in $ids; do
    if ! [[ "$id" =~ ^[a-f0-9]{64}$ ]]; then
      printf 'docker-smoke: warning: invalid %s identity; resource retained\n' "$kind" >&2
      result=1
      continue
    fi
    if [ "$CAPTURED" = 1 ] && [ "$MUTATION_ACTIVE" = 0 ]; then
      case " $known " in
        *" $id "*) ;;
        *) printf 'docker-smoke: warning: unrecognized %s %s; retained\n' "$kind" "$id" >&2; result=1; continue ;;
      esac
    fi
    if [ "$kind" = container ]; then
      actual=$(docker container inspect --format '{{.ID}}|{{index .Config.Labels "io.tribunal.smoke.run"}}|{{index .Config.Labels "com.docker.compose.project"}}' "$id" 2>>"$LOG") || actual=''
    else
      actual=$(docker network inspect --format '{{.ID}}|{{index .Labels "io.tribunal.smoke.run"}}|{{index .Labels "com.docker.compose.project"}}' "$id" 2>>"$LOG") || actual=''
    fi
    if [ "$actual" != "$id|$RUN_ID|$PROJECT" ]; then
      printf 'docker-smoke: warning: %s %s has unknown ownership; retained\n' "$kind" "$id" >&2
      result=1
      continue
    fi
    if [ "$action" = remove ]; then
      if [ "$kind" = container ]; then
        if ! docker container rm --force "$id" >>"$LOG" 2>&1; then
          printf 'docker-smoke: warning: could not remove verified %s %s\n' "$kind" "$id" >&2
          result=1
        fi
      else
        if ! docker network rm "$id" >>"$LOG" 2>&1; then
          printf 'docker-smoke: warning: could not remove verified %s %s\n' "$kind" "$id" >&2
          result=1
        fi
      fi
    fi
  done
  if [ "$action" = capture ] && [ "$result" -eq 0 ]; then
    if [ "$kind" = container ]; then
      KNOWN_CONTAINERS=$words
    else
      KNOWN_NETWORKS=$words
    fi
  fi
  return "$result"
}

image_resource() {
  local action=$1 by_tag='' by_label='' ids id actual expected result=0
  by_tag=$(list_images) || {
    printf 'docker-smoke: warning: could not list image by tag; ownership is unknown\n' >&2
    result=1
  }
  by_label=$(list_run_images) || {
    printf 'docker-smoke: warning: could not list image by run label; ownership is unknown\n' >&2
    result=1
  }
  ids=$(printf '%s\n%s\n' "$by_tag" "$by_label" | sort -u) || result=1
  if [ -z "$ids" ]; then
    if [ "$action" = verify ] || [ -n "$IMAGE_ID" ]; then
      printf 'docker-smoke: warning: expected image is missing or replaced\n' >&2
      return 1
    fi
    return "$result"
  fi
  for id in $ids; do
    if ! [[ "$id" =~ ^sha256:[a-f0-9]{64}$ ]]; then
      printf 'docker-smoke: warning: invalid image identity; retained\n' >&2
      result=1
      continue
    fi
    actual=$(docker image inspect --format '{{.ID}}|{{index .Config.Labels "io.tribunal.smoke.run"}}|{{json .RepoTags}}' "$id" 2>>"$LOG") || actual=''
    expected="$id|$RUN_ID|[\"$SMOKE_IMAGE\"]"
    if [ "$action" = remove ] && { [ "$actual" = "$id|$RUN_ID|null" ] || [ "$actual" = "$id|$RUN_ID|[]" ]; }; then
      expected=$actual
    fi
    if [ "$actual" != "$expected" ] || { [ -n "$IMAGE_ID" ] && [ "$IMAGE_ID" != "$id" ]; }; then
      printf 'docker-smoke: warning: image %s has unknown ownership; retained\n' "$id" >&2
      result=1
      continue
    fi
    IMAGE_ID=$id
    if [ "$action" = remove ] && ! docker image rm --no-prune "$id" >>"$LOG" 2>&1; then
      printf 'docker-smoke: warning: could not remove verified image %s\n' "$id" >&2
      result=1
    fi
  done
  return "$result"
}

# A public label complements the daemon-allocated name; it is not a secret.
RUN_ID=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
[[ "$RUN_ID" =~ ^[a-f0-9]{32}$ ]] || fail "cannot generate a run label"
VOLUME_CREATE_PENDING=1
VOLUME_NAME=$(docker volume create --label "io.tribunal.smoke.run=$RUN_ID" 2>>"$LOG") \
  || fail "volume creation failed; any unconfirmed volume is retained"
VOLUME_CREATE_PENDING=0
[[ "$VOLUME_NAME" =~ ^[a-f0-9]{64}$ ]] || fail "daemon returned an invalid volume name"
volume_info=$(docker volume inspect --format '{{.Name}}|{{index .Labels "io.tribunal.smoke.run"}}|{{.CreatedAt}}' "$VOLUME_NAME" 2>>"$LOG") \
  || fail "cannot confirm the allocated volume; it is retained"
case "$volume_info" in
  "$VOLUME_NAME|$RUN_ID|"*) VOLUME_CREATED=${volume_info#"$VOLUME_NAME|$RUN_ID|"} ;;
  *) fail "allocated volume label or identity does not match; it is retained" ;;
esac
[ -n "$VOLUME_CREATED" ] || fail "allocated volume has no creation time; it is retained"
VOLUME_CONFIRMED=1
PROJECT="tribunal-smoke-$VOLUME_NAME"
SMOKE_IMAGE="tribunal:smoke-$PROJECT"

# The external volume is managed separately; Compose cannot recreate or delete it.
: >"$FAKE_ENV"
cat >"$OVERRIDE_FILE" <<EOF
services:
  review:
    image: $SMOKE_IMAGE
    labels:
      io.tribunal.smoke.run: "$RUN_ID"
    build:
      labels:
        io.tribunal.smoke.run: "$RUN_ID"
volumes:
  data:
    external: true
    name: $VOLUME_NAME
networks:
  default:
    labels:
      io.tribunal.smoke.run: "$RUN_ID"
EOF

# Override both current and legacy names before Compose interpolation.
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

step() {
  printf '==> %s\n' "$*"
}

assert_exec() {
  local label=$1
  shift
  compose exec -T review "$@" >>"$LOG" 2>&1 || fail "$label"
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

verify_namespace() {
  verify_volume || fail "allocated volume changed or cannot be inspected"
  project_resources container verify || fail "container ownership cannot be confirmed"
  project_resources network verify || fail "network ownership cannot be confirmed"
  image_resource verify || fail "image ownership cannot be confirmed"
}

capture_namespace() {
  verify_volume || fail "allocated volume changed or cannot be inspected"
  project_resources container capture || fail "container ownership cannot be confirmed"
  project_resources network capture || fail "network ownership cannot be confirmed"
  image_resource verify || fail "image ownership cannot be confirmed"
  CAPTURED=1
  MUTATION_ACTIVE=0
}

step "validating Compose configuration for $PROJECT"
compose config --quiet >>"$LOG" 2>&1 || fail "docker compose config rejected the deployment files"

# Reject existing names and Compose project labels before building or starting.
for kind in container network; do
  existing=$(list_project_resources "$kind") || fail "cannot check existing ${kind}s"
  [ -z "$existing" ] || fail "existing $kind conflicts with the smoke project"
done
existing=$(list_images) || fail "cannot check the smoke image tag"
[ -z "$existing" ] || fail "existing image conflicts with the smoke tag"
existing=$(list_run_images) || fail "cannot check the smoke image run label"
[ -z "$existing" ] || fail "existing image conflicts with the smoke run label"
verify_volume || fail "allocated volume changed or cannot be inspected"

step "building $SMOKE_IMAGE"
BUILD_STARTED=1
compose build review >>"$LOG" 2>&1 || fail "docker compose build failed"
verify_namespace

step "checking server configuration"
RUN_STARTED=1
MUTATION_ACTIVE=1
compose run --no-deps -T review \
  --config /app/config/server.yaml --check-config >>"$LOG" 2>&1 \
  || fail "server --check-config failed"
capture_namespace

step "starting $PROJECT (bounded to 180s)"
verify_namespace
MUTATION_ACTIVE=1
compose up -d --wait --wait-timeout 180 >>"$LOG" 2>&1 \
  || fail "docker compose up did not become healthy within 180s"
capture_namespace

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
compose exec -T review sh -eu >>"$LOG" 2>&1 <<'EOF' \
  || fail "codebase-memory CLI could not index an isolated repository"
root=/app/data/cbm-smoke
mkdir -p "$root/home" "$root/other" "$root/base"
printf 'package a\n\nfunc Add(a, b int) int { return a + b }\n' >"$root/base/a.go"
HOME="$root/home" tribunal-codebase-memory-mcp cli --quiet index_repository "{\"repo_path\":\"$root/base\"}"
HOME="$root/home" tribunal-codebase-memory-mcp cli list_projects | grep -q cbm-smoke-base
! HOME="$root/other" tribunal-codebase-memory-mcp cli list_projects | grep -q cbm-smoke-base
EOF
servers=$(api GET '/api/v1/mcp-servers?scope=platform') || fail "platform MCP server list request failed"
case "$servers" in
  *'"name":"codebase-memory"'*) ;;
  *) fail "default platform MCP server codebase-memory is not registered" ;;
esac
server_id=$(printf '%s' "$servers" | grep -o '"id":"[^"]*"' | head -n 1 | cut -d'"' -f4)
[ -n "$server_id" ] || fail "cannot read the codebase-memory server id"
tools=$(api POST "/api/v1/mcp-servers/$server_id/test") \
  || fail "codebase-memory MCP did not start and list tools"
# Every read tool the default config binds must be advertised by the bundled
# binary; review time drops a missing one with only a warning.
default_tools=$(sed -n '/&codebase_memory_read_tools$/,/^ *review:/s/^ *- \([a-z_]*\)$/\1/p' "$CONFIG_FILE")
[ -n "$default_tools" ] || fail "cannot read the default codebase-memory tool list"
for tool in $default_tools; do
  case "$tools" in
    *"\"name\":\"$tool\""*) ;;
    *) fail "codebase-memory MCP does not list default tool $tool" ;;
  esac
done
for tool in index_repository delete_project ingest_traces manage_adr; do
  case "$tools" in
    *"\"name\":\"$tool\""*) fail "codebase-memory MCP exposes write tool $tool" ;;
  esac
done

step "recreating $PROJECT and checking volume persistence"
verify_namespace
MUTATION_ACTIVE=1
compose up -d --force-recreate --wait --wait-timeout 180 >>"$LOG" 2>&1 \
  || fail "recreated service was not healthy within 180s"
capture_namespace
marker=$(compose exec -T review cat /app/data/smoke-marker 2>>"$LOG") \
  || fail "cannot read the persisted marker"
[ "$marker" = "tribunal-smoke-marker" ] || fail "persisted marker changed after recreation"
assert_exec "database did not persist across recreation" \
  sh -eu -c 'test -f /app/data/review.db'

step "smoke checks passed"
