#!/usr/bin/env bash
# Creates, rotates or removes the monitoring token of one box.agent box, through
# the PlusClouds API (api.plusclouds.com/monitoring), so the caller needs a
# PlusClouds API token and no monitoring.server key.
#
# create   Makes the host (external type "llmbox", external id = the box ID) and
#          its box.agent check, then prints the two settings the box needs:
#              MONITOR_URL=https://monitoring.plusclouds.com/ingest/v1/<check id>
#              MONITOR_TOKEN=mpush_...
#          The token is shown once; only these two lines go to stdout, so
#          `eval "$(... create BOX_ID)"` or `>> box.env` works. Everything else goes to stderr.
#          Re-running for a box that already has a check refuses; use rotate.
# rotate   Replaces the token (the old one stops working at once), prints the same two lines.
# delete   Removes the check and the host (use when the box is deprovisioned;
#          otherwise its silence raises a CRITICAL incident).
#
# Usage:
#   PLUSCLOUDS_TOKEN=... scripts/box-monitor-token.sh create  BOX_ID
#   PLUSCLOUDS_TOKEN=... scripts/box-monitor-token.sh rotate  BOX_ID
#   PLUSCLOUDS_TOKEN=... scripts/box-monitor-token.sh delete  BOX_ID
#
# Environment:
#   PLUSCLOUDS_TOKEN  PlusClouds API token (required). Never printed.
#   API_URL           default https://api.plusclouds.com
#   INTERVAL          check interval in seconds, default 5 (the box.agent floor). If a server
#                     older than v0.13.1 enforces a higher tenant minimum, the script uses
#                     that minimum and says so (silence is detected after MISSED_COUNT x interval).
#   MISSED_COUNT      silent intervals before CRITICAL, default 3
#
# Needs: bash, curl, jq.

set -euo pipefail

API_URL="${API_URL:-https://api.plusclouds.com}"
INTERVAL="${INTERVAL:-5}"
MISSED_COUNT="${MISSED_COUNT:-3}"
BASE="${API_URL%/}/monitoring"

die() { echo "error: $*" >&2; exit 1; }
log() { echo "$*" >&2; }

usage() { sed -n '2,29p' "$0" | sed 's/^# \{0,1\}//' >&2; exit "${1:-1}"; }

[ $# -eq 2 ] || [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ] || usage 1
case "${1:-}" in -h|--help) usage 0 ;; esac
CMD="$1"
BOX_ID="$2"

[ -n "${PLUSCLOUDS_TOKEN:-}" ] || die "set PLUSCLOUDS_TOKEN to a PlusClouds API token"
command -v curl >/dev/null || die "curl is required"
command -v jq >/dev/null || die "jq is required"
[[ "$BOX_ID" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$ ]] || die "BOX_ID: letters, digits, . _ -, at most 128 characters"
[[ "$INTERVAL" =~ ^[0-9]+$ ]] && [ "$INTERVAL" -ge 1 ] || die "INTERVAL must be a whole number of seconds, at least 1"

# api METHOD PATH [JSON] sets STATUS and BODY. The token goes through a curl
# config on stdin, so it never appears in the process list.
api() {
  local method="$1" path="$2" data="${3:-}" out
  local args=(-sS -m 30 -w $'\n%{http_code}' -X "$method" -K - -H "Accept: application/json")
  if [ -n "$data" ]; then args+=(-H "Content-Type: application/json" --data "$data"); fi
  out="$(printf 'header = "Authorization: Bearer %s"\n' "$PLUSCLOUDS_TOKEN" | curl "${args[@]}" "$BASE$path")" \
    || die "cannot reach $BASE$path"
  STATUS="${out##*$'\n'}"
  BODY="${out%$'\n'*}"
}

fail() { # fail WHAT
  local msg
  msg="$(printf '%s' "$BODY" | jq -r '.error.message // .message // empty' 2>/dev/null || true)"
  die "$1: HTTP $STATUS${msg:+ - $msg}"
}

find_host() { # prints the host id for BOX_ID, or nothing
  api GET "/hosts?type=server"
  [ "$STATUS" = 200 ] || fail "list hosts"
  printf '%s' "$BODY" | jq -r --arg id "$BOX_ID" \
    '(.data | if type == "array" then . else (.items // []) end)[] | select(.external_id == $id) | .id' | head -n1
}

find_check() { # find_check HOST_ID prints the box.agent check id, or nothing
  api GET "/hosts/$1/checks"
  [ "$STATUS" = 200 ] || fail "list checks"
  printf '%s' "$BODY" | jq -r \
    '(.data | if type == "array" then . else (.items // []) end)[] | select(.plugin == "box.agent") | .id' | head -n1
}

print_settings() { # print_settings URL TOKEN
  printf 'MONITOR_URL=%s\nMONITOR_TOKEN=%s\n' "$1" "$2"
}

create_check() { # create_check HOST_ID [INTERVAL] sets STATUS and BODY
  local payload
  payload="$(jq -nc --arg box "$BOX_ID" --argjson interval "$2" --argjson missed "$MISSED_COUNT" '{
    name: "box", plugin: "box.agent", is_host_check: true, interval_seconds: $interval,
    config: {box_id: $box, missed_count: $missed},
    thresholds: [{metric: "gpu_temp_c", object: "*", warning: {op: ">", value: 85}}]}')"
  api POST "/hosts/$1/checks" "$payload"
}

case "$CMD" in
create)
  host="$(find_host)"
  if [ -z "$host" ]; then
    payload="$(jq -nc --arg id "$BOX_ID" '{name: $id, type: "server", external_id: $id, external_type: "llmbox"}')"
    api POST "/hosts" "$payload"
    [ "$STATUS" = 201 ] || fail "create host"
    host="$(printf '%s' "$BODY" | jq -r '.data.id')"
    log "created host $host"
  else
    log "host $host already exists"
    [ -z "$(find_check "$host")" ] || die "box $BOX_ID already has a box.agent check; use rotate to get a new token"
  fi

  interval="$INTERVAL"
  create_check "$host" "$interval"
  if [ "$STATUS" = 422 ]; then # the tenant's minimum interval is higher
    min="$(printf '%s' "$BODY" | jq -r '.error.message // ""' | sed -n 's/.*must be at least \([0-9][0-9]*\).*/\1/p')"
    if [ -n "$min" ] && [ "$min" -gt "$interval" ]; then
      log "warning: this tenant allows checks no faster than ${min}s; using ${min}s (silence is detected after $((MISSED_COUNT * min))s)."
      interval="$min"
      create_check "$host" "$interval"
    fi
  fi
  [ "$STATUS" = 201 ] || fail "create check"
  url="$(printf '%s' "$BODY" | jq -r '.data.push.ingest_url // empty')"
  token="$(printf '%s' "$BODY" | jq -r '.data.push_token // empty')"
  [ -n "$url" ] && [ -n "$token" ] || die "the API returned no ingest URL or token"
  log "created box.agent check $(printf '%s' "$BODY" | jq -r '.data.id') (interval ${interval}s)"
  print_settings "$url" "$token"
  ;;
rotate)
  host="$(find_host)"
  [ -n "$host" ] || die "no host for box $BOX_ID"
  check="$(find_check "$host")"
  [ -n "$check" ] || die "box $BOX_ID has no box.agent check; use create"
  api POST "/checks/$check/rotate-token"
  [ "$STATUS" = 200 ] || fail "rotate token"
  token="$(printf '%s' "$BODY" | jq -r '.data.push_token // empty')"
  url="$(printf '%s' "$BODY" | jq -r '.data.push.ingest_url // empty')"
  [ -n "$token" ] || die "the API returned no token"
  log "rotated: the old token stopped working; deploy the new one to the box"
  print_settings "$url" "$token"
  ;;
delete)
  host="$(find_host)"
  [ -n "$host" ] || die "no host for box $BOX_ID"
  check="$(find_check "$host")"
  if [ -n "$check" ]; then
    api DELETE "/checks/$check"
    [ "$STATUS" = 204 ] || [ "$STATUS" = 200 ] || fail "delete check"
  fi
  api DELETE "/hosts/$host"
  [ "$STATUS" = 204 ] || [ "$STATUS" = 200 ] || fail "delete host"
  log "deleted the check and host of box $BOX_ID"
  ;;
*) usage 1 ;;
esac
