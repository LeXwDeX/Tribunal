#!/usr/bin/env bash
# Repeatable concurrency smoke test for an existing Tribunal deployment.
#
# This script only triggers reviews and records their final usage; it does not
# create GitLab projects, branches or merge requests. Prepare one MR per
# project first, then pass the project paths and MR IIDs in the same order.
#
# Required:
#   TRIBUNAL_URL          e.g. http://192.168.33.110:18500
#   TRIBUNAL_ADMIN_TOKEN  platform admin bearer token
#   TRIBUNAL_USER_TOKEN   a user token that can manage the projects
#   PROJECTS              space-separated paths, e.g. "suntao/a suntao/b"
#   MR_IIDS               space-separated IIDs aligned with PROJECTS
#
# Optional:
#   GITLAB_TOKEN          only needed when a project is not registered yet
#   USER_ID               user to grant manage on each project (default 59)
#   POLL_INTERVAL         seconds between polls (default 5)
#   TIMEOUT               total seconds before giving up (default 900)
#   HTTP_TIMEOUT          seconds per HTTP request (default 30)
#
# Output: CSV rows on stdout, a summary on stderr. Secrets are never printed.
set -euo pipefail

: "${TRIBUNAL_URL:?set TRIBUNAL_URL}"
: "${TRIBUNAL_ADMIN_TOKEN:?set TRIBUNAL_ADMIN_TOKEN}"
: "${TRIBUNAL_USER_TOKEN:?set TRIBUNAL_USER_TOKEN}"
: "${PROJECTS:?set PROJECTS}"
: "${MR_IIDS:?set MR_IIDS}"
: "${USER_ID:=59}"
: "${POLL_INTERVAL:=5}"
: "${TIMEOUT:=900}"
: "${HTTP_TIMEOUT:=30}"

for bin in curl jq awk sort; do
  command -v "$bin" >/dev/null 2>&1 || { echo "missing $bin" >&2; exit 1; }
done

read -r -a project_list <<<"$PROJECTS"
read -r -a mr_list <<<"$MR_IIDS"
if [ "${#project_list[@]}" -ne "${#mr_list[@]}" ]; then
  echo "PROJECTS and MR_IIDS must have the same number of entries" >&2
  exit 1
fi

base="${TRIBUNAL_URL%/}"
api() {
  local method=$1 path=$2 token=$3 body=${4:-}
  if [ -n "$body" ]; then
    curl -fsS --connect-timeout 5 --max-time "$HTTP_TIMEOUT" -X "$method" "$base$path" \
      -H "Authorization: Bearer $token" \
      -H "Content-Type: application/json" \
      --data-binary "$body"
  else
    curl -fsS --connect-timeout 5 --max-time "$HTTP_TIMEOUT" -X "$method" "$base$path" \
      -H "Authorization: Bearer $token"
  fi
}

find_project_id() {
  local path=$1 cursor="" page id url
  while :; do
    url="/api/v1/admin/projects?limit=100"
    if [ -n "$cursor" ]; then
      url="$url&cursor=$(printf '%s' "$cursor" | jq -sRr @uri)"
    fi
    page=$(api GET "$url" "$TRIBUNAL_ADMIN_TOKEN")
    id=$(jq -r --arg path "$path" '.projects[]? | select(.path_with_namespace == $path) | .id' <<<"$page" | head -n1)
    if [ -n "$id" ]; then
      printf '%s' "$id"
      return
    fi
    cursor=$(jq -r '.next_cursor // empty' <<<"$page")
    [ -z "$cursor" ] && return
  done
}

register_project() {
  local path=$1 id
  id=$(find_project_id "$path" || true)
  if [ -n "$id" ]; then
    printf '%s' "$id"
    return
  fi
  if [ -z "${GITLAB_TOKEN:-}" ]; then
    echo "project $path is not registered and GITLAB_TOKEN is not set" >&2
    exit 1
  fi
  local payload
  payload=$(jq -n --arg path "$path" --arg token "$GITLAB_TOKEN" \
    '{project:$path, token:$token, auto_webhook:true}')
  api POST "/api/v1/projects" "$TRIBUNAL_ADMIN_TOKEN" "$payload" |
    jq -r '.project.id'
}

project_ids=()
for path in "${project_list[@]}"; do
  id=$(register_project "$path")
  if [ -z "$id" ] || [ "$id" = "null" ]; then
    echo "cannot resolve project id for $path" >&2
    exit 1
  fi
  project_ids+=("$id")
  api PUT "/api/v1/admin/projects/$id/grants/$USER_ID" "$TRIBUNAL_ADMIN_TOKEN" \
    '{"role":"manage"}' >/dev/null
done

triggered_at=$(date -u +%Y-%m-%dT%H:%M:%S)
pids=()
for i in "${!project_ids[@]}"; do
  (
    api POST "/api/v1/projects/${project_ids[$i]}/merge_requests/${mr_list[$i]}/reviews" \
      "$TRIBUNAL_USER_TOKEN" '{}' >/dev/null
  ) &
  pids+=("$!")
done
trigger_failed=0
for pid in "${pids[@]}"; do
  wait "$pid" || trigger_failed=1
done
if [ "$trigger_failed" -ne 0 ]; then
  echo "at least one review trigger failed" >&2
  exit 1
fi

deadline=$((SECONDS + TIMEOUT))
rows=""
while :; do
  rows=""
  for i in "${!project_ids[@]}"; do
    page=$(api GET "/api/v1/reviews?project_id=${project_ids[$i]}&mr_iid=${mr_list[$i]}&limit=100" "$TRIBUNAL_USER_TOKEN")
    row=$(jq -r --arg start "$triggered_at" '
      [.reviews[]? | select((.created_at[0:19]) >= $start)]
      | max_by(.created_at) // empty
      | [.project_id,.mr_iid,.status,(.verdict // ""),(.duration_ms // 0),
         .tokens_in,.tokens_out,.cost_micros] | @csv' <<<"$page")
    if [ -n "$row" ]; then
      rows+="$row"$'\n'
    fi
  done
  pending=$(printf '%s' "$rows" | awk -F, 'NF && $3 !~ /"(completed|failed|cancelled)"/ {n++} END {print n+0}')
  found=$(printf '%s' "$rows" | awk -F, 'NF {n++} END {print n+0}')
  if [ "$found" -ge "${#project_ids[@]}" ] && [ "$pending" -eq 0 ]; then
    break
  fi
  if [ "$SECONDS" -ge "$deadline" ]; then
    echo "timeout after ${TIMEOUT}s; last rows:" >&2
    printf '%s\n' "$rows" >&2
    exit 1
  fi
  sleep "$POLL_INTERVAL"
done

echo "project_id,mr_iid,status,verdict,duration_ms,tokens_in,tokens_out,cost_micros"
printf '%s' "$rows"

count=$(printf '%s' "$rows" | awk -F, 'NF {n++} END {print n+0}')
if [ "$count" -eq 0 ]; then
  echo "no reviews matched the requested projects" >&2
  exit 1
fi
failed=$(printf '%s' "$rows" | awk -F, 'NF {s=$3; gsub(/"/,"",s); if (s != "completed") n++} END {print n+0}')
durations=$(printf '%s' "$rows" | awk -F, 'NF {d=$5; gsub(/"/,"",d); print d}' | sort -n)
p50=$(printf '%s\n' "$durations" | sed -n "$(( (count + 1) / 2 ))p")
p95=$(printf '%s\n' "$durations" | sed -n "$(( (count * 95 + 99) / 100 ))p")
read -r tokens_in tokens_out cost_micros < <(
  printf '%s\n' "$rows" | awk -F, 'NF {tin+=$6; tout+=$7; cost+=$8} END {printf "%d %d %d", tin, tout, cost}'
)
printf 'reviews=%d failed=%d p50_ms=%s p95_ms=%s tokens_in=%s tokens_out=%s cost_micros=%s\n' \
  "$count" "$failed" "$p50" "$p95" "$tokens_in" "$tokens_out" "$cost_micros" >&2
