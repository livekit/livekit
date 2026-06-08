#!/usr/bin/env bash
#
# Run one FlexFEC test "leg" on the Linux box: start the SFU, a publisher (robot), and a
# subscriber (operator), let them run, then print a summary of SFU FEC recovery + the
# subscriber's frame-latency / video-quality stats.
#
# Two modes:
#   - loopback (default): everything on 127.0.0.1, no impairment. Smoke test.
#   - netns (USE_NETNS=1): publisher runs inside the `robot` netns (set up + shaped via
#     netns.sh), SFU + subscriber run in the root netns reaching the SFU at 10.200.0.1.
#     This models a lossy robot uplink with a clean operator downlink.
#
# Env:
#   LABEL          leg label for the banner
#   DURATION       seconds to run (default 35)
#   USE_NETNS      1 to run publisher in the robot netns (default 0 = loopback)
#   SFU_BIN        path to SFU binary (default /tmp/flexfec-livekit-server)
#   RUST_DIR       rust-sdks5 dir (default ~/workspace/rust-sdks5)
#   LK_DIR         livekit-flexfec dir (default ~/workspace/livekit-flexfec)
#   PUB_EXTRA      extra publisher args (e.g. "--max-playout-delay 30")
set -uo pipefail

LABEL="${LABEL:-leg}"
DURATION="${DURATION:-35}"
USE_NETNS="${USE_NETNS:-0}"
SFU_BIN="${SFU_BIN:-/tmp/flexfec-livekit-server}"
RUST_DIR="${RUST_DIR:-$HOME/workspace/rust-sdks5}"
LK_DIR="${LK_DIR:-$HOME/workspace/livekit-flexfec}"
PUB_EXTRA="${PUB_EXTRA:-}"

if [[ "$USE_NETNS" == "1" ]]; then
  SFU_HOST="10.200.0.1"
else
  SFU_HOST="127.0.0.1"
fi

# FEC=on (default) uses flexfec-linux.yaml (publisher: true). FEC=off writes a sibling
# config with publisher: false so the SFU does not negotiate uplink FlexFEC and the robot
# falls back to RTX-only -- the A/B baseline.
BASE_CONFIG="$LK_DIR/scripts/flexfec/flexfec-linux.yaml"
if [[ "${FEC:-on}" == "off" ]]; then
  CONFIG="/tmp/flexfec-linux-fecoff.yaml"
  sed -E 's/^( *publisher:) *true/\1 false/' "$BASE_CONFIG" > "$CONFIG"
else
  CONFIG="$BASE_CONFIG"
fi

LOGDIR=/tmp/flexfec-logs
mkdir -p "$LOGDIR"
SFU_LOG="$LOGDIR/sfu.log"
PUB_LOG="$LOGDIR/pub.log"
SUB_LOG="$LOGDIR/sub.log"

cleanup() {
  pkill -f flexfec-livekit-server 2>/dev/null
  pkill -f "target/debug/publisher" 2>/dev/null
  pkill -f "target/debug/subscriber" 2>/dev/null
}
cleanup
sleep 1

echo "==================== LEG: $LABEL (netns=$USE_NETNS, ${DURATION}s) ===================="

# In netns mode, apply netem shaping to the robot uplink. NETEM_LOSS/% NETEM_OWD ms, plus
# optional NETEM_JITTER ms and NETEM_CORR % for bursty (5G-like) loss/jitter.
if [[ "$USE_NETNS" == "1" ]]; then
  NETEM_LOSS="${NETEM_LOSS:-0}"
  NETEM_OWD="${NETEM_OWD:-0}"
  if [[ "$NETEM_LOSS" != "0" || "$NETEM_OWD" != "0" ]]; then
    bash "$LK_DIR/scripts/flexfec/netns.sh" shape "$NETEM_LOSS" "$NETEM_OWD" "${NETEM_JITTER:-0}" "${NETEM_CORR:-0}"
  else
    bash "$LK_DIR/scripts/flexfec/netns.sh" clear
  fi
fi

# --- SFU (root netns) ---
# In-SFU software impairment (used when netem/root is unavailable). All no-ops unless set.
#   PUB_LOSS / PUB_DELAY_MS        robot uplink (publisher->SFU) loss + one-way delay
#   DOWNLINK_LOSS / DOWNLINK_DELAY_MS / UPLINK_DELAY_MS   operator downlink + NACK delay
( cd "$LK_DIR" && \
  LK_PUB_LOSS="${PUB_LOSS:-0}" LK_PUB_DELAY_MS="${PUB_DELAY_MS:-0}" \
  LK_DOWNLINK_LOSS="${DOWNLINK_LOSS:-0}" LK_DOWNLINK_DELAY_MS="${DOWNLINK_DELAY_MS:-0}" \
  LK_UPLINK_DELAY_MS="${UPLINK_DELAY_MS:-0}" \
  "$SFU_BIN" --dev --config "$CONFIG" > "$SFU_LOG" 2>&1 ) &
sleep 3

export LIVEKIT_URL="ws://$SFU_HOST:7880"
export LIVEKIT_API_KEY=devkey
export LIVEKIT_API_SECRET=secret
export RUST_LOG="${RUST_LOG:-info}"
ROOM=flexfec-test

PUB="$RUST_DIR/target/debug/publisher"
SUB="$RUST_DIR/target/debug/subscriber"

# --- Publisher (robot) ---
PUB_ARGS=(--flex-fec --room-name "$ROOM" --identity flexfec-pub --test-pattern 1 --attach-timestamp --attach-frame-id)
# shellcheck disable=SC2206
[[ -n "$PUB_EXTRA" ]] && PUB_ARGS+=($PUB_EXTRA)

if [[ "$USE_NETNS" == "1" ]]; then
  # netns.sh sudoes ip/tc internally; do NOT wrap it in sudo (the script isn't allowlisted).
  bash "$LK_DIR/scripts/flexfec/netns.sh" exec env \
    LIVEKIT_URL="$LIVEKIT_URL" LIVEKIT_API_KEY=devkey LIVEKIT_API_SECRET=secret RUST_LOG="$RUST_LOG" \
    "$PUB" "${PUB_ARGS[@]}" > "$PUB_LOG" 2>&1 &
else
  "$PUB" "${PUB_ARGS[@]}" > "$PUB_LOG" 2>&1 &
fi
sleep 4

# --- Subscriber (operator, root netns) ---
# The subscriber opens an eframe/wgpu window (frame-latency stats are produced by its render
# loop), so it needs a display. The box has a real X server on :1; use it unless DISPLAY is
# already set. xvfb-run is used as a fallback if available.
SUB_DISPLAY="${DISPLAY:-:1}"
SUB_XAUTH="${XAUTHORITY:-/run/user/$(id -u)/gdm/Xauthority}"
DISPLAY="$SUB_DISPLAY" XAUTHORITY="$SUB_XAUTH" "$SUB" --room-name "$ROOM" --identity flexfec-sub > "$SUB_LOG" 2>&1 &

sleep "$DURATION"
cleanup
sleep 1

echo "-- SFU FlexFEC recovery (publisher uplink), last 3 --"
grep -iE 'flexfec recovery stats' "$SFU_LOG" 2>/dev/null | tail -3 | sed -E 's/.*"fecPacketsReceived"/fecRecv/; s/, "fecReportedReceived.*//'
echo "-- SFU flexfec decoder enabled? --"
grep -iE 'flexfec decoder enabled' "$SFU_LOG" 2>/dev/null | tail -1
echo "-- frame latency, last 6 windows --"
grep -iE 'frame latency' "$SUB_LOG" 2>/dev/null | tail -6
echo "-- video quality (last) --"
grep -iE 'Video quality' "$SUB_LOG" 2>/dev/null | tail -1
echo "-- jitter buffer (last) --"
grep -iE 'jitter buffer' "$SUB_LOG" 2>/dev/null | tail -1
echo "============================================================================"
