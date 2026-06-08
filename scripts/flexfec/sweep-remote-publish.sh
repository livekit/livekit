#!/usr/bin/env bash
#
# Focused tele-op publish-leg benchmark:
#   - this computer runs local_video publisher + subscriber
#   - DC-Linux runs the SFU
#   - the SFU applies publish-side packet loss only, with no added delay
#
# This models a robot publishing over an already-real public/LAN path where the link RTT is
# real and should not be inflated by the test harness. Subscriber/downlink metrics are still
# collected, but FlexFEC generation toward the subscriber is disabled by default.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

REMOTE="${REMOTE:-dc@DC-Linux}"
REMOTE_LK_DIR="${REMOTE_LK_DIR:-/home/dc/workspace/livekit-flexfec-bench}"
REMOTE_GO_BIN="${REMOTE_GO_BIN:-/home/dc/go1.26/bin/go}"
REMOTE_SFU_BIN="${REMOTE_SFU_BIN:-/tmp/flexfec-livekit-server}"
REMOTE_WORK_DIR="${REMOTE_WORK_DIR:-/tmp/flexfec-remote-publish}"
REMOTE_BIND_ADDRESSES="${REMOTE_BIND_ADDRESSES:-10.104.0.88 100.69.171.8 127.0.0.1}"
REMOTE_ICE_INCLUDES="${REMOTE_ICE_INCLUDES:-10.104.0.0/16 100.64.0.0/10 127.0.0.0/8}"
SSH_OPTS="${SSH_OPTS:--o ControlPath=/tmp/dc-linux-ctrl}"

RUST_DIR="${RUST_DIR:-$REPO_ROOT/../rust-sdks5}"
PUB_BIN="${PUB_BIN:-$RUST_DIR/target/debug/publisher}"
SUB_BIN="${SUB_BIN:-$RUST_DIR/target/debug/subscriber}"
SFU_HOST="${SFU_HOST:-DC-Linux}"
LIVEKIT_URL="${LIVEKIT_URL:-ws://$SFU_HOST:7880}"

OUT_DIR="${OUT_DIR:-$SCRIPT_DIR/results/remote-publish-$(date +%Y%m%d-%H%M%S)}"
SUMMARY_CSV="$OUT_DIR/summary.csv"

DURATION="${DURATION:-60}"
SFU_BOOT="${SFU_BOOT:-3}"
SUB_BOOT="${SUB_BOOT:-4}"
PUB_BOOT="${PUB_BOOT:-4}"
ROOM_PREFIX="${ROOM_PREFIX:-teleop-publish}"
RUST_LOG="${RUST_LOG:-info}"
PAYLOAD_TYPE="${PAYLOAD_TYPE:-49}"
DROP_LOG="${DROP_LOG:-1}"
REBUILD_RUST="${REBUILD_RUST:-0}"
DRY_RUN="${DRY_RUN:-0}"

FEC_CONFIGS="${FEC_CONFIGS:-rtx:off:0:0 fec6x1:on:6:1 fec8x1:on:8:1 fec5x1:on:5:1}"
LOSS_PCTS="${LOSS_PCTS:-0.1 0.5 1 2 5}"
BURST_MS="${BURST_MS:-100 200 500}"
BURST_GAP_MS="${BURST_GAP_MS:-2000}"

PUB_EXTRA="${PUB_EXTRA:---codec vp8 --encoder software}"
SUB_EXTRA="${SUB_EXTRA:-}"

declare -a LOCAL_PIDS=()
REMOTE_SFU_PID=""
REMOTE_CASE_DIR=""

log() {
  printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*"
}

die() {
  echo "error: $*" >&2
  exit 1
}

ssh_remote() {
  # shellcheck disable=SC2086
  ssh $SSH_OPTS "$REMOTE" "$@"
}

rsync_from_remote() {
  local src="$1"
  local dst="$2"
  # shellcheck disable=SC2086
  rsync -az -e "ssh $SSH_OPTS" "$REMOTE:$src" "$dst"
}

safe_name() {
  printf '%s' "$1" | tr -c 'A-Za-z0-9_.=-' '_'
}

csv_quote() {
  local s="${1:-}"
  s="${s//\"/\"\"}"
  printf '"%s"' "$s"
}

shell_quote() {
  printf '%q' "$1"
}

loss_fraction() {
  awk "BEGIN { printf \"%.6f\", $1 / 100 }"
}

write_summary_header() {
  mkdir -p "$OUT_DIR"
  cat >"$SUMMARY_CSV" <<'CSV'
timestamp,profile,fec_label,fec_enabled,num_media_packets,num_fec_packets,loss_pct,burst_ms,gap_ms,sfu_packets_recovered,sfu_drop_count,sfu_marker_count,subscriber_fec_accepted_total,video_quality_last,frame_latency_last,case_dir
CSV
}

write_remote_config() {
  local fec_enabled="$1"
  local num_media="$2"
  local num_fec="$3"
  local remote_config="$4"
  local bind_addresses_yaml=""
  local ice_includes_yaml=""

  if [[ "$fec_enabled" == "false" ]]; then
    num_media=5
    num_fec=1
  fi

  local bind_address
  for bind_address in $REMOTE_BIND_ADDRESSES; do
    bind_addresses_yaml+="  - $bind_address"$'\n'
  done

  local ice_include
  for ice_include in $REMOTE_ICE_INCLUDES; do
    ice_includes_yaml+="      - $ice_include"$'\n'
  done

  ssh_remote "cat > '$remote_config' <<'EOF'
port: 7880

bind_addresses:
$bind_addresses_yaml

rtc:
  udp_port: 7882
  tcp_port: 7881
  use_external_ip: false
  enable_loopback_candidate: true
  ips:
    includes:
$ice_includes_yaml

  flexfec:
    subscriber: false
    publisher: $fec_enabled
    payload_type: $PAYLOAD_TYPE
    num_media_packets: $num_media
    num_fec_packets: $num_fec

logging:
  level: debug
  json: false
EOF"
}

stop_local_processes() {
  set +e
  if [[ "${#LOCAL_PIDS[@]}" -gt 0 ]]; then
    kill "${LOCAL_PIDS[@]}" 2>/dev/null
    sleep 1
    kill -9 "${LOCAL_PIDS[@]}" 2>/dev/null
    wait "${LOCAL_PIDS[@]}" 2>/dev/null
  fi
  LOCAL_PIDS=()
  set -e
}

stop_remote_sfu() {
  set +e
  if [[ -n "${REMOTE_SFU_PID:-}" ]]; then
    ssh_remote "kill '$REMOTE_SFU_PID' 2>/dev/null || true; sleep 1; kill -9 '$REMOTE_SFU_PID' 2>/dev/null || true; wait '$REMOTE_SFU_PID' 2>/dev/null || true" >/dev/null 2>&1
    REMOTE_SFU_PID=""
  fi
  ssh_remote "pkill -f '$REMOTE_SFU_BIN' 2>/dev/null || true" >/dev/null 2>&1
  set -e
}

cleanup_all() {
  set +e
  stop_local_processes
  stop_remote_sfu
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
  local profile="$1"
  local fec_label="$2"
  local fec_enabled="$3"
  local num_media="$4"
  local num_fec="$5"
  local loss="$6"
  local burst_ms="$7"
  local gap_ms="$8"
  local case_dir="$9"

  local sfu_log="$case_dir/sfu.log"
  local sub_log="$case_dir/subscriber.log"
  local sfu_recovery_line video_quality frame_latency accepted_line recovered accepted drops markers

  sfu_recovery_line="$(last_line 'flexfec recovery stats' "$sfu_log")"
  video_quality="$(last_line 'Video quality' "$sub_log")"
  frame_latency="$(last_line 'frame latency' "$sub_log")"
  accepted_line="$(last_line 'accepted_recovery_payload_total|accepted_total|Video FEC recovery payload accepted' "$sub_log")"

  recovered="$(extract_key_number 'packetsRecovered' "$sfu_recovery_line")"
  accepted="$(extract_key_number 'accepted_recovery_payload_total' "$accepted_line")"
  if [[ -z "$accepted" ]]; then
    accepted="$(extract_key_number 'accepted_total' "$accepted_line")"
  fi
  drops="$(grep -c 'uplink impairment dropped RTP packet' "$sfu_log" 2>/dev/null || true)"
  markers="$(grep -c 'uplink impairment received frame marker' "$sfu_log" 2>/dev/null || true)"

  {
    printf '%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,' \
      "$(date -Iseconds)" "$profile" "$fec_label" "$fec_enabled" "$num_media" "$num_fec" \
      "$loss" "$burst_ms" "$gap_ms" "${recovered:-0}" "${drops:-0}" "${markers:-0}" "${accepted:-0}"
    csv_quote "$video_quality"
    printf ','
    csv_quote "$frame_latency"
    printf ','
    csv_quote "$case_dir"
    printf '\n'
  } >>"$SUMMARY_CSV"
}

build_artifacts() {
  [[ -d "$RUST_DIR" ]] || die "rust-sdks checkout not found at $RUST_DIR (set RUST_DIR)"

  log "building remote SFU: $REMOTE:$REMOTE_SFU_BIN"
  ssh_remote "cd '$REMOTE_LK_DIR' && '$REMOTE_GO_BIN' build -o '$REMOTE_SFU_BIN' ./cmd/server"

  if [[ "$REBUILD_RUST" == "1" || ! -x "$PUB_BIN" || ! -x "$SUB_BIN" ]]; then
    log "building local_video publisher/subscriber in $RUST_DIR"
    (cd "$RUST_DIR" && cargo build -p local_video --features desktop --bin publisher --bin subscriber)
  else
    log "using existing local_video binaries in $RUST_DIR/target/debug"
  fi
}

make_profiles() {
  for loss in $LOSS_PCTS; do
    for burst in $BURST_MS; do
      printf 'loss%s_b%s:%s:%s:%s\n' \
        "$(printf '%s' "$loss" | tr '.' 'p')" "$burst" "$loss" "$burst" "$BURST_GAP_MS"
    done
  done
}

run_case() {
  local config_spec="$1"
  local profile_spec="$2"
  local fec_label fec_mode num_media num_fec profile loss burst_ms gap_ms
  IFS=: read -r fec_label fec_mode num_media num_fec <<<"$config_spec"
  IFS=: read -r profile loss burst_ms gap_ms <<<"$profile_spec"

  local fec_enabled=false
  [[ "$fec_mode" == "on" ]] && fec_enabled=true

  local case_name case_dir remote_case config room pub_args
  case_name="$(safe_name "${profile}_${fec_label}")"
  case_dir="$OUT_DIR/$case_name"
  remote_case="$REMOTE_WORK_DIR/$case_name"
  config="$remote_case/sfu.yaml"
  room="${ROOM_PREFIX}-${case_name}"
  mkdir -p "$case_dir"

  log "case: profile=$profile fec=$fec_label loss=${loss}% burst=${burst_ms}ms gap=${gap_ms}ms url=$LIVEKIT_URL"

  if [[ "$DRY_RUN" == "1" ]]; then
    return
  fi

  stop_local_processes
  stop_remote_sfu
  LOCAL_PIDS=()

  ssh_remote "mkdir -p '$remote_case'"
  write_remote_config "$fec_enabled" "$num_media" "$num_fec" "$config"

  local loss_frac
  loss_frac="$(loss_fraction "$loss")"
  local remote_start_cmd
  remote_start_cmd="cd $(shell_quote "$REMOTE_LK_DIR") || exit; env LK_PUB_LOSS=$(shell_quote "$loss_frac") LK_PUB_DELAY_MS=0 LK_PUB_BURST_MS=$(shell_quote "$burst_ms") LK_PUB_GAP_MS=$(shell_quote "$gap_ms") LK_PUB_DROP_LOG=$(shell_quote "$DROP_LOG") $(shell_quote "$REMOTE_SFU_BIN") --dev --config $(shell_quote "$config") > $(shell_quote "$remote_case/sfu.log") 2>&1 < /dev/null & pid=\$!; disown \"\$pid\"; echo \"\$pid\""
  REMOTE_SFU_PID="$(
    ssh_remote "bash -lc $(shell_quote "$remote_start_cmd")"
  )"
  sleep "$SFU_BOOT"

  LIVEKIT_URL="$LIVEKIT_URL" LIVEKIT_API_KEY=devkey LIVEKIT_API_SECRET=secret RUST_LOG="$RUST_LOG" \
    "$SUB_BIN" --room-name "$room" --identity "${case_name}-sub" $SUB_EXTRA >"$case_dir/subscriber.log" 2>&1 &
  LOCAL_PIDS+=("$!")
  sleep "$SUB_BOOT"

  pub_args=(--room-name "$room" --identity "${case_name}-pub" --test-pattern 1 --attach-timestamp --attach-frame-id)
  if [[ "$fec_enabled" == "true" ]]; then
    pub_args=(--flex-fec "${pub_args[@]}")
  fi
  LIVEKIT_URL="$LIVEKIT_URL" LIVEKIT_API_KEY=devkey LIVEKIT_API_SECRET=secret RUST_LOG="$RUST_LOG" \
    "$PUB_BIN" "${pub_args[@]}" $PUB_EXTRA >"$case_dir/publisher.log" 2>&1 &
  LOCAL_PIDS+=("$!")
  sleep "$PUB_BOOT"

  sleep "$DURATION"

  stop_local_processes
  stop_remote_sfu
  rsync_from_remote "$remote_case/" "$case_dir/"
  grep 'uplink impairment dropped RTP packet' "$case_dir/sfu.log" >"$case_dir/dropped-packets.log" 2>/dev/null || true
  grep 'uplink impairment received frame marker' "$case_dir/sfu.log" >"$case_dir/frame-markers.log" 2>/dev/null || true

  summarize_case "$profile" "$fec_label" "$fec_enabled" "$num_media" "$num_fec" "$loss" "$burst_ms" "$gap_ms" "$case_dir"
}

main() {
  build_artifacts
  write_summary_header

  log "results: $OUT_DIR"
  for profile_spec in $(make_profiles); do
    for config_spec in $FEC_CONFIGS; do
      run_case "$config_spec" "$profile_spec"
    done
  done

  cleanup_all
  log "summary: $SUMMARY_CSV"
}

trap cleanup_all EXIT INT TERM
main "$@"
