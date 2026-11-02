#!/usr/bin/env bash
# End-to-end tests for scripts/loadtest.sh against a local mock Tribunal API.
# Usage: bash scripts/loadtest_test.sh
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
script="$root/scripts/loadtest.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"; [ -n "${server_pid:-}" ] && kill "$server_pid" 2>/dev/null || true' EXIT

fail=0
pass=0

start_server() {
  local mode=$1
  port=$((20000 + RANDOM % 20000))
  MOCK_MODE="$mode" python3 - "$port" >"$tmp/server.log" 2>&1 <<'PY' &
import json
import os
import sys
from datetime import datetime, timedelta, timezone
from http.server import BaseHTTPRequestHandler, HTTPServer

mode = os.environ.get("MOCK_MODE", "normal")
port = int(sys.argv[1])
state = {"baseline": {}, "triggered": set(), "grants": 0, "get_after_trigger": 0}


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def send_json(self, code, body):
        data = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        if self.path == "/__stats":
            return self.send_json(200, {"grants": state["grants"]})
        if self.path == "/api/v1/me":
            return self.send_json(200, {"id": 59})
        if self.path.startswith("/api/v1/admin/projects"):
            return self.send_json(200, {
                "projects": [
                    {"id": 1, "path_with_namespace": "g/a"},
                    {"id": 2, "path_with_namespace": "g/b"},
                ],
                "next_cursor": None,
            })
        if self.path.startswith("/api/v1/reviews"):
            query = self.path.split("?", 1)[1] if "?" in self.path else ""
            params = dict(p.split("=", 1) for p in query.split("&") if "=" in p)
            key = (params.get("project_id"), params.get("mr_iid"))
            if key not in state["baseline"]:
                state["baseline"][key] = True
                old = []
                if mode == "baseline":
                    old = [{
                        "id": "old",
                        "project_id": int(key[0]),
                        "mr_iid": int(key[1]),
                        "status": "completed",
                        "verdict": "PASS",
                        "duration_ms": 1000,
                        "tokens_in": 9999,
                        "tokens_out": 9999,
                        "cost_micros": 9999,
                        "created_at": "2026-01-01T00:00:00Z",
                    }]
                return self.send_json(200, {"reviews": old, "next_cursor": None})
            if key not in state["triggered"]:
                return self.send_json(200, {"reviews": [], "next_cursor": None})
            state["get_after_trigger"] += 1
            if mode == "retry" and state["get_after_trigger"] == 1:
                return self.send_json(503, {"error": "busy"})
            status = "cancelled" if mode == "cancelled" else "completed"
            now = datetime.now(timezone.utc)
            new_created = now.isoformat().replace("+00:00", "Z")
            old_created = (now + timedelta(seconds=1)).isoformat().replace("+00:00", "Z")
            reviews = []
            if mode == "baseline":
                reviews.append({
                    "id": "old",
                    "project_id": int(key[0]),
                    "mr_iid": int(key[1]),
                    "status": "completed",
                    "verdict": "PASS",
                    "duration_ms": 1000,
                    "tokens_in": 9999,
                    "tokens_out": 9999,
                    "cost_micros": 9999,
                    "created_at": old_created,
                })
            reviews.append({
                "id": "new",
                "project_id": int(key[0]),
                "mr_iid": int(key[1]),
                "status": status,
                "verdict": "" if status == "cancelled" else "PASS",
                "duration_ms": 0 if status == "cancelled" else 15000,
                "tokens_in": 100,
                "tokens_out": 50,
                "cost_micros": 123,
                "created_at": new_created,
            })
            return self.send_json(200, {"reviews": reviews, "next_cursor": None})
        return self.send_json(404, {"error": "not found"})

    def do_POST(self):
        if mode == "trigger_fail":
            return self.send_json(422, {"error": {"code": "gitlab_rejected"}})
        parts = self.path.split("/")
        if len(parts) >= 8 and parts[1:4] == ["api", "v1", "projects"]:
            state["triggered"].add((parts[4], parts[6]))
        return self.send_json(202, {})

    def do_PUT(self):
        state["grants"] += 1
        return self.send_json(200, {})


HTTPServer(("127.0.0.1", port), Handler).serve_forever()
PY
  server_pid=$!
  base="http://127.0.0.1:$port"
  for _ in $(seq 1 50); do
    if curl -fsS "$base/__stats" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.1
  done
  echo "mock server failed to start: $(cat "$tmp/server.log")" >&2
  exit 1
}

stop_server() {
  [ -n "${server_pid:-}" ] && kill "$server_pid" 2>/dev/null || true
  wait "$server_pid" 2>/dev/null || true
  server_pid=
}

run_script() {
  local out=$1 err=$2; shift 2
  set +e
  env TIMEOUT="${TEST_TIMEOUT:-15}" POLL_INTERVAL="${TEST_POLL_INTERVAL:-1}" \
    "$@" bash "$script" >"$out" 2>"$err"
  code=$?
  set -e
  return 0
}

check() {
  local name=$1 condition=$2 detail=$3
  if eval "$condition"; then
    pass=$((pass + 1))
    printf 'ok   %s\n' "$name"
  else
    fail=$((fail + 1))
    printf 'FAIL %s: %s\n' "$name" "$detail"
  fi
}

# 1. normal: two projects complete, summary and throughput must be present.
start_server normal
run_script "$tmp/out1" "$tmp/err1" \
  TRIBUNAL_URL="$base" TRIBUNAL_ADMIN_TOKEN=x TRIBUNAL_USER_TOKEN=y \
  PROJECTS="g/a g/b" MR_IIDS="1 1"
check "normal exit 0" '[ "$code" -eq 0 ]' "exit=$code stderr=$(cat "$tmp/err1")"
check "normal summary" 'grep -q "reviews=2 completed=2 failed=0 cancelled=0" "$tmp/err1"' "$(cat "$tmp/err1")"
check "normal throughput" 'grep -q "reviews_per_sec=" "$tmp/err1"' "$(cat "$tmp/err1")"
check "normal csv" '[ "$(wc -l < "$tmp/out1")" -eq 3 ]' "lines=$(wc -l < "$tmp/out1")"
check "no grant by default" '[ "$(curl -fsS "$base/__stats" | jq -r .grants)" -eq 0 ]' "grants=$(curl -fsS "$base/__stats")"
stop_server

# 2. retry: first poll after trigger gets 503, then succeeds.
start_server retry
run_script "$tmp/out2" "$tmp/err2" \
  TRIBUNAL_URL="$base" TRIBUNAL_ADMIN_TOKEN=x TRIBUNAL_USER_TOKEN=y \
  PROJECTS="g/a" MR_IIDS="1" HTTP_RETRIES=2 POLL_INTERVAL=1
check "retry exit 0" '[ "$code" -eq 0 ]' "exit=$code stderr=$(cat "$tmp/err2")"
check "retry summary" 'grep -q "completed=1" "$tmp/err2"' "$(cat "$tmp/err2")"
stop_server

# 3. cancelled: separate counter and non-zero exit.
start_server cancelled
run_script "$tmp/out3" "$tmp/err3" \
  TRIBUNAL_URL="$base" TRIBUNAL_ADMIN_TOKEN=x TRIBUNAL_USER_TOKEN=y \
  PROJECTS="g/a" MR_IIDS="1"
check "cancelled exit 1" '[ "$code" -eq 1 ]' "exit=$code stderr=$(cat "$tmp/err3")"
check "cancelled counter" 'grep -q "cancelled=1" "$tmp/err3"' "$(cat "$tmp/err3")"
check "cancelled not failed" 'grep -q "failed=0" "$tmp/err3"' "$(cat "$tmp/err3")"
stop_server

# 4. duplicate project/MR pair is rejected before triggering.
start_server normal
run_script "$tmp/out4" "$tmp/err4" \
  TRIBUNAL_URL="$base" TRIBUNAL_ADMIN_TOKEN=x TRIBUNAL_USER_TOKEN=y \
  PROJECTS="g/a g/a" MR_IIDS="1 1"
check "duplicate exit 1" '[ "$code" -eq 1 ]' "exit=$code"
check "duplicate message" 'grep -qi "duplicate" "$tmp/err4"' "$(cat "$tmp/err4")"
stop_server

# 5. baseline: an older completed review with the same second must be ignored.
start_server baseline
run_script "$tmp/out5" "$tmp/err5" \
  TRIBUNAL_URL="$base" TRIBUNAL_ADMIN_TOKEN=x TRIBUNAL_USER_TOKEN=y \
  PROJECTS="g/a" MR_IIDS="1"
check "baseline exit 0" '[ "$code" -eq 0 ]' "exit=$code stderr=$(cat "$tmp/err5")"
check "baseline ignores old review" 'grep -q "tokens_in=100 " "$tmp/err5"' "$(cat "$tmp/err5")"
stop_server

# 6. explicit GRANT_MANAGE grants the resolved user once.
start_server normal
run_script "$tmp/out6" "$tmp/err6" \
  TRIBUNAL_URL="$base" TRIBUNAL_ADMIN_TOKEN=x TRIBUNAL_USER_TOKEN=y \
  PROJECTS="g/a" MR_IIDS="1" GRANT_MANAGE=1
check "grant exit 0" '[ "$code" -eq 0 ]' "exit=$code stderr=$(cat "$tmp/err6")"
check "grant count" '[ "$(curl -fsS "$base/__stats" | jq -r .grants)" -eq 1 ]' "grants=$(curl -fsS "$base/__stats")"
stop_server

# 7. trigger failure is reported once, without interleaved curl output.
start_server trigger_fail
run_script "$tmp/out7" "$tmp/err7" \
  TRIBUNAL_URL="$base" TRIBUNAL_ADMIN_TOKEN=x TRIBUNAL_USER_TOKEN=y \
  PROJECTS="g/a" MR_IIDS="1"
check "trigger failure exit 1" '[ "$code" -eq 1 ]' "exit=$code"
check "trigger failure message" 'grep -q "trigger g/a !1 failed" "$tmp/err7"' "$(cat "$tmp/err7")"
stop_server

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
