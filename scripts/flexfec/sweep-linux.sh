#!/usr/bin/env bash
#
# Run a Linux FlexFEC-vs-RTX benchmark sweep.
#
# The sweep runs two independent legs:
#   - uplink: publisher/robot -> SFU, shaped through the robot netns veth.
#   - downlink: SFU -> subscriber/operator, shaped on loopback by SFU UDP source port.
#
# Each case writes raw logs and appends one row to summary.csv. Defaults are intentionally
# small enough to run overnight-ish while still covering RTX-only, low-latency FEC, and
# stronger FEC under short fade and longer handoff-style burst loss.
#
# Run from this repo on Linux:
#   scripts/flexfec/sweep-linux.sh
#
# Useful overrides:
#   RUST_DIR=~/workspace/rust-sdks5
#   GO_BIN=~/go1.26/bin/go
#   OUT_DIR=/tmp/flexfec-results
#   DURATION=60
#   SCENARIOS="uplink downlink"
#   FEC_CONFIGS="rtx:off:0:0 fec5x1:on:5:1 fec10x2:on:10:2"
#   LOSS_PROFILES="fade:35:250:1750:40:10:70 handoff:80:750:5000:60:20:85"
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

LK_DIR="${LK_DIR:-$REPO_ROOT}"
RUST_DIR="${RUST_DIR:-$REPO_ROOT/../rust-sdks5}"
SFU_BIN="${SFU_BIN:-/tmp/flexfec-livekit-server}"
OUT_DIR="${OUT_DIR:-$SCRIPT_DIR/results/$(date +%Y%m%d-%H%M%S)}"

DURATION="${DURATION:-45}"
SFU_BOOT="${SFU_BOOT:-3}"
PUB_BOOT="${PUB_BOOT:-4}"
SUB_BOOT="${SUB_BOOT:-5}"

SCENARIOS="${SCENARIOS:-uplink downlink}"
FEC_CONFIGS="${FEC_CONFIGS:-rtx:off:0:0 fec5x1:on:5:1 fec8x1:on:8:1 fec10x2:on:10:2}"
LOSS_PROFILES="${LOSS_PROFILES:-fade-short:35:250:1750:40:10:70 fade-long:70:750:5000:60:20:85}"

ROOM_PREFIX="${ROOM_PREFIX:-flexfec-bench}"
RUST_LOG="${RUST_LOG:-info}"
PAYLOAD_TYPE="${PAYLOAD_TYPE:-49}"
GO_BIN="${GO_BIN:-go}"
REBUILD_RUST="${REBUILD_RUST:-0}"
DRY_RUN="${DRY_RUN:-0}"

PUB_BIN="$RUST_DIR/target/debug/publisher"
SUB_BIN="$RUST_DIR/target/debug/subscriber"
SUMMARY_CSV="$OUT_DIR/summary.csv"

declare -a CASE_PIDS=()
IMPAIR_PID=""
NETNS_ACTIVE=0
CURRENT_LEG=""

if [[ "$(uname -s)" != "Linux" ]]; then
  echo "error: sweep-linux.sh must run on Linux" >&2
  exit 1
fi

if [[ "$(id -u)" == "0" ]]; then
  SUDO=()
elif [[ -n "${SUDO_ASKPASS:-}" ]]; then
  SUDO=(sudo -A)
else
  SUDO=(sudo)
fi

log() {
  printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*"
}

die() {
  echo "error: $*" >&2
  exit 1
}

safe_name() {
  printf '%s' "$1" | tr -c 'A-Za-z0-9_.=-' '_'
}

ms_to_seconds() {
  awk "BEGIN { printf \"%.3f\", $1 / 1000 }"
}

csv_quote() {
  local s="${1:-}"
  s="${s//\"/\"\"}"
  printf '"%s"' "$s"
}

run_netns() {
  "${SUDO[@]}" bash "$SCRIPT_DIR/netns.sh" "$@"
}

run_netem() {
  "${SUDO[@]}" bash "$SCRIPT_DIR/netem.sh" "$@"
}

case_contains_scenario() {
  local want="$1"
  for scenario in $SCENARIOS; do
    [[ "$scenario" == "$want" ]] && return 0
  done
  return 1
}

prepare_sudo() {
  if [[ "${#SUDO[@]}" -gt 0 ]]; then
    log "checking sudo access for tc/ip netem setup"
    "${SUDO[@]}" -v
  fi
}

build_artifacts() {
  [[ -d "$RUST_DIR" ]] || die "rust-sdks checkout not found at $RUST_DIR (set RUST_DIR)"

  log "building SFU: $SFU_BIN"
  (cd "$LK_DIR" && "$GO_BIN" build -o "$SFU_BIN" ./cmd/server)

  if [[ "$REBUILD_RUST" == "1" || ! -x "$PUB_BIN" || ! -x "$SUB_BIN" ]]; then
    log "building local_video publisher/subscriber in $RUST_DIR"
    (cd "$RUST_DIR" && cargo build -p local_video --features desktop --bin publisher --bin subscriber)
  else
    log "using existing local_video binaries in $RUST_DIR/target/debug"
  fi
}

write_summary_header() {
  mkdir -p "$OUT_DIR"
  cat >"$SUMMARY_CSV" <<'CSV'
timestamp,leg,profile,fec_label,fec_enabled,num_media_packets,num_fec_packets,loss_pct,burst_ms,gap_ms,one_way_delay_ms,jitter_ms,loss_correlation_pct,sfu_packets_recovered,subscriber_fec_accepted_total,video_quality_last,frame_latency_last,case_dir
CSV
}

write_config() {
  local leg="$1"
  local fec_enabled="$2"
  local num_media="$3"
  local num_fec="$4"
  local config="$5"

  local publisher=false
  local subscriber=false
  local bind_addresses=""
  local includes=""

  if [[ "$leg" == "uplink" ]]; then
    publisher="$fec_enabled"
    bind_addresses=$'bind_addresses:\n  - 10.200.0.1\n  - 127.0.0.1'
    includes=$'      - 10.200.0.0/24\n      - 127.0.0.0/8'
  else
    subscriber="$fec_enabled"
    bind_addresses=$'bind_addresses:\n  - 127.0.0.1'
    includes=$'      - 127.0.0.0/8'
  fi

  if [[ "$fec_enabled" == "false" ]]; then
    num_media=5
    num_fec=1
  fi

  cat >"$config" <<EOF
port: 7880

$bind_addresses

rtc:
  udp_port: 7882
  tcp_port: 7881
  use_external_ip: false
  enable_loopback_candidate: true
  ips:
    includes:
$includes

  flexfec:
    subscriber: $subscriber
    publisher: $publisher
    payload_type: $PAYLOAD_TYPE
    num_media_packets: $num_media
    num_fec_packets: $num_fec

logging:
  level: debug
  json: false
EOF
}

stop_impairment() {
  set +e
  if [[ -n "${IMPAIR_PID:-}" ]]; then
    kill "$IMPAIR_PID" 2>/dev/null
    for _ in {1..20}; do
      kill -0 "$IMPAIR_PID" 2>/dev/null || break
      sleep 0.1
    done
    kill -9 "$IMPAIR_PID" 2>/dev/null
    wait "$IMPAIR_PID" 2>/dev/null
    IMPAIR_PID=""
  fi

  case "$CURRENT_LEG" in
    uplink) run_netns clear >/dev/null 2>&1 ;;
    downlink) run_netem stop >/dev/null 2>&1 ;;
  esac
  set -e
}

stop_case_processes() {
  set +e
  if [[ "${#CASE_PIDS[@]}" -gt 0 ]]; then
    kill "${CASE_PIDS[@]}" 2>/dev/null
    sleep 1
    kill -9 "${CASE_PIDS[@]}" 2>/dev/null
    wait "${CASE_PIDS[@]}" 2>/dev/null
  fi
  CASE_PIDS=()

  pkill -f "$SFU_BIN" 2>/dev/null
  pkill -f "$PUB_BIN" 2>/dev/null
  pkill -f "$SUB_BIN" 2>/dev/null
  set -e
}

cleanup_all() {
  set +e
  stop_impairment
  set +e
  stop_case_processes
  set +e
  if [[ "$NETNS_ACTIVE" == "1" ]]; then
    run_netns down >/dev/null 2>&1
  fi
}

start_uplink_burst() {
  local case_dir="$1"
  local loss="$2"
  local burst_ms="$3"
  local gap_ms="$4"
  local owd_ms="$5"
  local jitter_ms="$6"
  local corr_pct="$7"
  local burst_s gap_s
  burst_s="$(ms_to_seconds "$burst_ms")"
  gap_s="$(ms_to_seconds "$gap_ms")"

  (
    trap 'exit 0' INT TERM
    while true; do
      run_netns shape "$loss" "$owd_ms" "$jitter_ms" "$corr_pct"
      sleep "$burst_s"
      run_netns shape 0 "$owd_ms" "$jitter_ms" 0
      sleep "$gap_s"
    done
  ) >"$case_dir/netem.log" 2>&1 &
  IMPAIR_PID="$!"
}

start_downlink_burst() {
  local case_dir="$1"
  local loss="$2"
  local burst_ms="$3"
  local gap_ms="$4"
  local owd_ms="$5"

  run_netem burst "$loss" "$burst_ms" "$gap_ms" down "$owd_ms" >"$case_dir/netem.log" 2>&1 &
  IMPAIR_PID="$!"
}

start_impairment() {
  local leg="$1"
  local case_dir="$2"
  local loss="$3"
  local burst_ms="$4"
  local gap_ms="$5"
  local owd_ms="$6"
  local jitter_ms="$7"
  local corr_pct="$8"

  CURRENT_LEG="$leg"
  if [[ "$leg" == "uplink" ]]; then
    start_uplink_burst "$case_dir" "$loss" "$burst_ms" "$gap_ms" "$owd_ms" "$jitter_ms" "$corr_pct"
  else
    start_downlink_burst "$case_dir" "$loss" "$burst_ms" "$gap_ms" "$owd_ms"
  fi
}

last_line() {
  local pattern="$1"
  local file="$2"
  grep -iE "$pattern" "$file" 2>/dev/null | tail -1 || true
}

extract_key_number() {
  local key="$1"
  local line="$2"
  printf '%s' "$line" | sed -nE "s/.*${key}\"?[:= ]+([0-9]+).*/\1/p"
}

summarize_case() {
  local leg="$1"
  local profile="$2"
  local fec_label="$3"
  local fec_enabled="$4"
  local num_media="$5"
  local num_fec="$6"
  local loss="$7"
  local burst_ms="$8"
  local gap_ms="$9"
  local owd_ms="${10}"
  local jitter_ms="${11}"
  local corr_pct="${12}"
  local case_dir="${13}"

  local sfu_log="$case_dir/sfu.log"
  local sub_log="$case_dir/subscriber.log"
  local sfu_recovery_line video_quality frame_latency accepted_line recovered accepted

  sfu_recovery_line="$(last_line 'flexfec recovery stats' "$sfu_log")"
  video_quality="$(last_line 'Video quality' "$sub_log")"
  frame_latency="$(last_line 'frame latency' "$sub_log")"
  accepted_line="$(last_line 'accepted_recovery_payload_total|accepted_total|Video FEC recovery payload accepted' "$sub_log")"

  recovered="$(extract_key_number 'packetsRecovered' "$sfu_recovery_line")"
  accepted="$(extract_key_number 'accepted_recovery_payload_total' "$accepted_line")"
  if [[ -z "$accepted" ]]; then
    accepted="$(extract_key_number 'accepted_total' "$accepted_line")"
  fi

  {
    printf '%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,' \
      "$(date -Iseconds)" "$leg" "$profile" "$fec_label" "$fec_enabled" \
      "$num_media" "$num_fec" "$loss" "$burst_ms" "$gap_ms" "$owd_ms" "$jitter_ms" "$corr_pct" \
      "${recovered:-0}" "${accepted:-0}"
    csv_quote "$video_quality"
    printf ','
    csv_quote "$frame_latency"
    printf ','
    csv_quote "$case_dir"
    printf '\n'
  } >>"$SUMMARY_CSV"
}

run_case() {
  local leg="$1"
  local config_spec="$2"
  local profile_spec="$3"
  local fec_label fec_mode num_media num_fec
  local profile loss burst_ms gap_ms owd_ms jitter_ms corr_pct

  IFS=: read -r fec_label fec_mode num_media num_fec <<<"$config_spec"
  IFS=: read -r profile loss burst_ms gap_ms owd_ms jitter_ms corr_pct <<<"$profile_spec"

  local fec_enabled=false
  [[ "$fec_mode" == "on" ]] && fec_enabled=true

  local case_name case_dir config sfu_host room
  case_name="$(safe_name "${leg}_${profile}_${fec_label}")"
  case_dir="$OUT_DIR/$case_name"
  config="$case_dir/sfu.yaml"
  room="${ROOM_PREFIX}-${case_name}"
  mkdir -p "$case_dir"
  write_config "$leg" "$fec_enabled" "$num_media" "$num_fec" "$config"

  if [[ "$leg" == "uplink" ]]; then
    sfu_host="10.200.0.1"
    run_netns clear >/dev/null 2>&1 || true
  else
    sfu_host="127.0.0.1"
    run_netem stop >/dev/null 2>&1 || true
  fi

  log "case: leg=$leg profile=$profile fec=$fec_label loss=${loss}% burst=${burst_ms}ms gap=${gap_ms}ms owd=${owd_ms}ms"

  if [[ "$DRY_RUN" == "1" ]]; then
    return
  fi

  stop_case_processes
  CASE_PIDS=()
  IMPAIR_PID=""
  CURRENT_LEG=""

  (
    cd "$LK_DIR"
    "$SFU_BIN" --dev --config "$config"
  ) >"$case_dir/sfu.log" 2>&1 &
  CASE_PIDS+=("$!")
  sleep "$SFU_BOOT"

  local url="ws://$sfu_host:7880"
  local pub_args=(--flex-fec --room-name "$room" --identity "${case_name}-pub" --test-pattern 1 --attach-timestamp --attach-frame-id)
  if [[ "$leg" == "uplink" ]]; then
    run_netns exec env \
      LIVEKIT_URL="$url" LIVEKIT_API_KEY=devkey LIVEKIT_API_SECRET=secret RUST_LOG="$RUST_LOG" \
      "$PUB_BIN" "${pub_args[@]}" >"$case_dir/publisher.log" 2>&1 &
  else
    LIVEKIT_URL="$url" LIVEKIT_API_KEY=devkey LIVEKIT_API_SECRET=secret RUST_LOG="$RUST_LOG" \
      "$PUB_BIN" "${pub_args[@]}" >"$case_dir/publisher.log" 2>&1 &
  fi
  CASE_PIDS+=("$!")
  sleep "$PUB_BOOT"

  local sub_display sub_xauth
  sub_display="${DISPLAY:-${SUB_DISPLAY:-:1}}"
  sub_xauth="${XAUTHORITY:-${SUB_XAUTHORITY:-/run/user/$(id -u)/gdm/Xauthority}}"
  DISPLAY="$sub_display" XAUTHORITY="$sub_xauth" \
    LIVEKIT_URL="$url" LIVEKIT_API_KEY=devkey LIVEKIT_API_SECRET=secret RUST_LOG="$RUST_LOG" \
    "$SUB_BIN" --room-name "$room" --identity "${case_name}-sub" >"$case_dir/subscriber.log" 2>&1 &
  CASE_PIDS+=("$!")
  sleep "$SUB_BOOT"

  start_impairment "$leg" "$case_dir" "$loss" "$burst_ms" "$gap_ms" "$owd_ms" "$jitter_ms" "$corr_pct"
  sleep "$DURATION"

  stop_impairment
  stop_case_processes
  summarize_case "$leg" "$profile" "$fec_label" "$fec_enabled" "$num_media" "$num_fec" \
    "$loss" "$burst_ms" "$gap_ms" "$owd_ms" "$jitter_ms" "$corr_pct" "$case_dir"
}

main() {
  prepare_sudo
  build_artifacts
  write_summary_header

  if case_contains_scenario uplink; then
    log "setting up robot netns for uplink benchmarks"
    run_netns up
    NETNS_ACTIVE=1
  fi

  log "results: $OUT_DIR"
  for scenario in $SCENARIOS; do
    case "$scenario" in
      uplink | downlink) ;;
      *) die "unknown scenario '$scenario' (use uplink/downlink)" ;;
    esac
    for profile_spec in $LOSS_PROFILES; do
      for config_spec in $FEC_CONFIGS; do
        run_case "$scenario" "$config_spec" "$profile_spec"
      done
    done
  done

  cleanup_all
  log "summary: $SUMMARY_CSV"
}

trap cleanup_all EXIT INT TERM
main "$@"
