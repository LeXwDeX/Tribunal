#!/usr/bin/env bash
# Repeatable concurrency smoke test for an existing Tribunal deployment.
#
# This script only triggers reviews and records their final usage; it does not
# create GitLab projects, branches or merge requests. Prepare one MR per
# project first, then pass the project paths and MR IIDs in the same order.
#
# Required:
#   TRIBUNAL_URL          e.g. http://127.0.0.1:8080
#   TRIBUNAL_ADMIN_TOKEN  platform admin bearer token
#   TRIBUNAL_USER_TOKEN   a user token that can manage the projects
#   PROJECTS              space-separated paths, e.g. "group/a group/b"
#   MR_IIDS               space-separated IIDs aligned with PROJECTS
#
# Optional:
#   GITLAB_TOKEN          only needed when a project is not registered yet
#   GRANT_MANAGE          set to 1 to grant manage to USER_ID (default 0)
#   USER_ID               user to grant manage; resolved from /api/v1/me when omitted
#   POLL_INTERVAL         seconds between polls (default 5)
#   TIMEOUT               total seconds before giving up (default 900)
#   HTTP_TIMEOUT          seconds per HTTP request (default 30)
#   HTTP_RETRIES          GET attempts for transient errors (default 3)
#
# Output: CSV rows on stdout, a summary on stderr. Secrets are never printed.
set -euo pipefail

die() { printf '%s\n' "$*" >&2; exit 1; }

positive_int() {
  case "$1" in ''|*[!0-9]*) return 1 ;; esac
  [ "$1" -gt 0 ]
}

: "${TRIBUNAL_URL:?set TRIBUNAL_URL}"
: "${TRIBUNAL_ADMIN_TOKEN:?set TRIBUNAL_ADMIN_TOKEN}"
: "${TRIBUNAL_USER_TOKEN:?set TRIBUNAL_USER_TOKEN}"
: "${PROJECTS:?set PROJECTS}"
: "${MR_IIDS:?set MR_IIDS}"
: "${USER_ID:=}"
: "${GRANT_MANAGE:=0}"
: "${POLL_INTERVAL:=5}"
: "${TIMEOUT:=900}"
: "${HTTP_TIMEOUT:=30}"
: "${HTTP_RETRIES:=3}"

for bin in curl jq awk sort mktemp; do
  command -v "$bin" >/dev/null 2>&1 || die "missing $bin"
done

positive_int "$POLL_INTERVAL" || die "POLL_INTERVAL must be a positive integer"
positive_int "$TIMEOUT" || die "TIMEOUT must be a positive integer"
positive_int "$HTTP_TIMEOUT" || die "HTTP_TIMEOUT must be a positive integer"
positive_int "$HTTP_RETRIES" || die "HTTP_RETRIES must be a positive integer"
case "$GRANT_MANAGE" in 0|1) ;; *) die "GRANT_MANAGE must be 0 or 1" ;; esac

base="${TRIBUNAL_URL%/}"
while [ "${base%/}" != "$base" ]; do
  base="${base%/}"
done
case "$base" in
  http://*|https://*) ;;
  *) die "TRIBUNAL_URL must start with http:// or https://" ;;
esac
case "$base" in
  *'?'*|*'#'*|*'@'*) die "TRIBUNAL_URL must not contain query, fragment or userinfo" ;;
esac

read -r -a project_list <<<"$PROJECTS"
read -r -a mr_list <<<"$MR_IIDS"
[ "${#project_list[@]}" -gt 0 ] || die "PROJECTS must contain at least one project"
[ "${#project_list[@]}" -eq "${#mr_list[@]}" ] || die "PROJECTS and MR_IIDS must have the same number of entries"
for iid in "${mr_list[@]}"; do
  positive_int "$iid" || die "MR_IIDS must contain positive integers"
done

pairs=""
for i in "${!project_list[@]}"; do
  pairs+="${project_list[$i]}|${mr_list[$i]}"$'\n'
done
duplicates=$(printf '%s' "$pairs" | sort | uniq -d)
[ -z "$duplicates" ] || die "duplicate project/MR pair: $(printf '%s' "$duplicates" | tr '\n' ' ')"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

api_once() { # method path token [body-or-@file]
  local method=$1 path=$2 token=$3 body=${4:-}
  if [ -n "$body" ]; then
    if [ "${body#@}" != "$body" ]; then
      printf 'Authorization: Bearer %s\n' "$token" | curl -q -fsS --globoff \
        --connect-timeout 5 --max-time "$HTTP_TIMEOUT" -X "$method" "$base$path" \
        -H @- -H "Content-Type: application/json" --data-binary "@${body#@}"
    else
      printf 'Authorization: Bearer %s\n' "$token" | curl -q -fsS --globoff \
        --connect-timeout 5 --max-time "$HTTP_TIMEOUT" -X "$method" "$base$path" \
        -H @- -H "Content-Type: application/json" --data-binary "$body"
    fi
  else
    printf 'Authorization: Bearer %s\n' "$token" | curl -q -fsS --globoff \
      --connect-timeout 5 --max-time "$HTTP_TIMEOUT" -X "$method" "$base$path" -H @-
  fi
}

api_get() { # path token
  local path=$1 token=$2 attempt=1 status=0
  while [ "$attempt" -le "$HTTP_RETRIES" ]; do
    api_once GET "$path" "$token" >"$tmp/response" && {
      cat "$tmp/response"
      return 0
    }
    status=$?
    if [ "$attempt" -lt "$HTTP_RETRIES" ]; then
      sleep 1
    fi
    attempt=$((attempt + 1))
  done
  printf 'GET %s failed after %d attempts (curl exit %d)\n' "$path" "$HTTP_RETRIES" "$status" >&2
  return "$status"
}

find_project_id() {
  local path=$1 cursor="" page id url
  while :; do
    url="/api/v1/admin/projects?limit=100"
    if [ -n "$cursor" ]; then
      url="$url&cursor=$(printf '%s' "$cursor" | jq -sRr @uri)"
    fi
    page=$(api_get "$url" "$TRIBUNAL_ADMIN_TOKEN") || return 1
    jq -e '.projects | type == "array"' >/dev/null <<<"$page" || die "admin project list returned an unexpected response"
    id=$(jq -r --arg path "$path" 'first(.projects[]? | select(.path_with_namespace == $path) | .id) // empty' <<<"$page")
    if [ -n "$id" ]; then
      printf '%s' "$id"
      return 0
    fi
    cursor=$(jq -r '.next_cursor // empty' <<<"$page")
    [ -z "$cursor" ] && return 0
  done
}

register_project() {
  local path=$1 id
  id=$(find_project_id "$path") || die "cannot list projects for $path"
  if [ -n "$id" ]; then
    printf '%s' "$id"
    return
  fi
  [ -n "${GITLAB_TOKEN:-}" ] || die "project $path is not registered and GITLAB_TOKEN is not set"
  GITLAB_TOKEN="$GITLAB_TOKEN" jq -n --arg path "$path" \
    '{project:$path, token:$ENV.GITLAB_TOKEN, auto_webhook:false}' >"$tmp/body"
  chmod 600 "$tmp/body"
  api_once POST "/api/v1/projects" "$TRIBUNAL_ADMIN_TOKEN" "@$tmp/body" | jq -r '.project.id'
}

if [ -z "$USER_ID" ] && [ "$GRANT_MANAGE" = "1" ]; then
  me=$(api_get "/api/v1/me" "$TRIBUNAL_USER_TOKEN") || die "cannot resolve USER_ID from TRIBUNAL_USER_TOKEN; set USER_ID explicitly"
  USER_ID=$(jq -r '.id // empty' <<<"$me")
  positive_int "$USER_ID" || die "cannot resolve a positive USER_ID from TRIBUNAL_USER_TOKEN; set USER_ID explicitly"
fi
if [ -n "$USER_ID" ]; then
  positive_int "$USER_ID" || die "USER_ID must be a positive integer"
fi

started_epoch=$(date +%s)
project_ids=()
for path in "${project_list[@]}"; do
  id=$(register_project "$path") || exit 1
  case "$id" in ''|*[!0-9]*) die "cannot resolve project id for $path" ;; esac
  project_ids+=("$id")
done
[ "${#project_ids[@]}" -gt 0 ] || die "no projects resolved"

if [ "$GRANT_MANAGE" = "1" ]; then
  [ -n "$USER_ID" ] || die "GRANT_MANAGE=1 requires USER_ID"
  for id in "${project_ids[@]}"; do
    if ! api_once PUT "/api/v1/admin/projects/$id/grants/$USER_ID" "$TRIBUNAL_ADMIN_TOKEN" \
      '{"role":"manage"}' >/dev/null 2>"$tmp/grant-$id.err"; then
      detail=$(head -c 200 "$tmp/grant-$id.err" 2>/dev/null | tr '\n' ' ')
      die "cannot grant manage on project $id: ${detail:-curl error}"
    fi
  done
fi

for i in "${!project_ids[@]}"; do
  baseline=$(api_get "/api/v1/reviews?project_id=${project_ids[$i]}&mr_iid=${mr_list[$i]}&limit=100" \
    "$TRIBUNAL_USER_TOKEN") || die "cannot read baseline reviews for ${project_list[$i]}"
  jq -c '[.reviews[]?.id]' <<<"$baseline" >"$tmp/baseline-$i.json"
done

triggered=""
failed_triggers=0
trigger_errors=""
pids=""
for i in "${!project_ids[@]}"; do
  (
    api_once POST "/api/v1/projects/${project_ids[$i]}/merge_requests/${mr_list[$i]}/reviews" \
      "$TRIBUNAL_USER_TOKEN" '{}' >/dev/null 2>"$tmp/trigger-$i.err"
  ) &
  pids+="$! $i"$'\n'
done
while read -r pid idx; do
  [ -n "$pid" ] || continue
  if wait "$pid"; then
    triggered+="$idx "
  else
    failed_triggers=$((failed_triggers + 1))
    detail=$(head -c 200 "$tmp/trigger-$idx.err" 2>/dev/null | tr '\n' ' ')
    trigger_errors+="trigger ${project_list[$idx]} !${mr_list[$idx]} failed: ${detail:-curl error}"$'\n'
  fi
done <<<"$pids"
if [ -n "$trigger_errors" ]; then
  printf '%s' "$trigger_errors" >&2
fi
[ -n "$triggered" ] || {
  printf 'triggers_failed=%d\n' "$failed_triggers" >&2
  die "all review triggers failed"
}

print_rows() {
  if [ ! -s "$tmp/rows.tsv" ]; then
    printf 'project_id,mr_iid,status,verdict,duration_ms,tokens_in,tokens_out,cost_micros\n'
    return
  fi
  awk -F'\t' 'BEGIN{OFS=","; print "project_id,mr_iid,status,verdict,duration_ms,tokens_in,tokens_out,cost_micros"}
    NF {
      for (i = 1; i <= NF; i++) {
        gsub(/"/, "\"\"", $i)
        if ($i ~ /[",]/) $i = "\"" $i "\""
      }
      print
    }' "$tmp/rows.tsv"
}

deadline=$((SECONDS + TIMEOUT))
rows_ready=0
while :; do
  : >"$tmp/rows.tsv"
  all_terminal=1
  for idx in $triggered; do
    page=$(api_get "/api/v1/reviews?project_id=${project_ids[$idx]}&mr_iid=${mr_list[$idx]}&limit=100" \
      "$TRIBUNAL_USER_TOKEN") || {
      print_rows
      printf 'polling failed; partial results above\n' >&2
      exit 1
    }
    row=$(jq -r --argjson baseline "$(cat "$tmp/baseline-$idx.json")" '
      [.reviews[]? | select(.id as $id | ($baseline | index($id)) == null)]
      | max_by(.created_at) // empty
      | [.project_id,.mr_iid,.status,(.verdict // ""),(.duration_ms // 0),
         .tokens_in,.tokens_out,.cost_micros] | @tsv' <<<"$page") || {
      print_rows
      printf 'invalid polling response for %s\n' "${project_list[$idx]}" >&2
      exit 1
    }
    if [ -n "$row" ]; then
      printf '%s\n' "$row" >>"$tmp/rows.tsv"
      status=$(printf '%s' "$row" | awk -F'\t' '{print $3}')
      case "$status" in
        completed|failed|cancelled) ;;
        *) all_terminal=0 ;;
      esac
    else
      all_terminal=0
    fi
  done
  found=$(awk -F'\t' 'NF{n++} END{print n+0}' "$tmp/rows.tsv")
  expected=$(printf '%s' "$triggered" | wc -w | tr -d ' ')
  if [ "$all_terminal" -eq 1 ] && [ "$found" -ge "$expected" ]; then
    rows_ready=1
    break
  fi
  if [ "$SECONDS" -ge "$deadline" ]; then
    print_rows
    printf 'timeout after %ds; partial results above\n' "$TIMEOUT" >&2
    exit 1
  fi
  sleep "$POLL_INTERVAL"
done
[ "$rows_ready" -eq 1 ] || exit 1

print_rows

count=$(awk -F'\t' 'NF{n++} END{print n+0}' "$tmp/rows.tsv")
[ "$count" -gt 0 ] || die "no reviews matched the requested projects"

completed=$(awk -F'\t' 'NF && $3 == "completed"{n++} END{print n+0}' "$tmp/rows.tsv")
failed=$(awk -F'\t' 'NF && $3 == "failed"{n++} END{print n+0}' "$tmp/rows.tsv")
cancelled=$(awk -F'\t' 'NF && $3 == "cancelled"{n++} END{print n+0}' "$tmp/rows.tsv")
failure_rate=$(awk -v bad="$((failed + cancelled))" -v n="$count" \
  'BEGIN{ if (n == 0) print "0.0"; else printf "%.1f", bad * 100 / n }')

durations=$(awk -F'\t' 'NF && $3 == "completed" && $5 + 0 > 0 {print $5}' "$tmp/rows.tsv" | sort -n)
valid=$(printf '%s\n' "$durations" | awk 'NF{n++} END{print n+0}')
if [ "$valid" -gt 0 ]; then
  p50=$(printf '%s\n' "$durations" | sed -n "$(( (valid + 1) / 2 ))p")
  p95=$(printf '%s\n' "$durations" | sed -n "$(( (valid * 95 + 99) / 100 ))p")
else
  p50=0
  p95=0
fi

totals=$(awk -F'\t' 'NF {tin+=$6; tout+=$7; cost+=$8} END{printf "%d %d %d", tin, tout, cost}' "$tmp/rows.tsv")
read -r tokens_in tokens_out cost_micros <<<"$totals"

lock_stats=$(api_get "/api/v1/system/lock-stats" "$TRIBUNAL_ADMIN_TOKEN" 2>/dev/null) || lock_stats=""
if [ -n "$lock_stats" ] && jq -e '.sqlite.wait_count != null and .workspace.wait_count != null' >/dev/null 2>&1 <<<"$lock_stats"; then
  sqlite_wait_count=$(jq -r '.sqlite.wait_count' <<<"$lock_stats")
  sqlite_wait_ms=$(jq -r '.sqlite.wait_duration_ms' <<<"$lock_stats")
  workspace_wait_count=$(jq -r '.workspace.wait_count' <<<"$lock_stats")
  workspace_wait_ms=$(jq -r '.workspace.wait_duration_ms' <<<"$lock_stats")
  workspace_max_wait_ms=$(jq -r '.workspace.max_wait_ms' <<<"$lock_stats")
else
  sqlite_wait_count="n/a"
  sqlite_wait_ms="n/a"
  workspace_wait_count="n/a"
  workspace_wait_ms="n/a"
  workspace_max_wait_ms="n/a"
fi

finished_epoch=$(date +%s)
wall_s=$((finished_epoch - started_epoch))
[ "$wall_s" -gt 0 ] || wall_s=1
reviews_per_sec=$(awk -v n="$count" -v s="$wall_s" 'BEGIN{ printf "%.2f", n / s }')

printf 'reviews=%d completed=%d failed=%d cancelled=%d triggers_failed=%d failure_rate=%s%% p50_ms=%s p95_ms=%s wall_s=%d reviews_per_sec=%s tokens_in=%d tokens_out=%d cost_micros=%d sqlite_wait_count=%s sqlite_wait_ms=%s workspace_wait_count=%s workspace_wait_ms=%s workspace_max_wait_ms=%s\n' \
  "$count" "$completed" "$failed" "$cancelled" "$failed_triggers" "$failure_rate" \
  "$p50" "$p95" "$wall_s" "$reviews_per_sec" "$tokens_in" "$tokens_out" "$cost_micros" \
  "$sqlite_wait_count" "$sqlite_wait_ms" "$workspace_wait_count" "$workspace_wait_ms" "$workspace_max_wait_ms" >&2

status_code=0
[ "$failed_triggers" -eq 0 ] || status_code=1
[ "$failed" -eq 0 ] || status_code=1
[ "$cancelled" -eq 0 ] || status_code=1
exit "$status_code"
