#!/usr/bin/env bash
#
# Run the rust-sdks local_video subscriber against the local dev SFU and surface
# FlexFEC stats. The subscriber logs "Video FEC ..." lines as repair packets arrive
# and recovery payload is accepted.
#
# Usage:
#   scripts/flexfec/run-subscriber.sh [extra subscriber args...]
#
# Env overrides:
#   RUST_SDKS_DIR   path to the rust-sdks5 checkout (default: ../rust-sdks5)
#   LIVEKIT_URL     default ws://127.0.0.1:7880
#   LIVEKIT_API_KEY / LIVEKIT_API_SECRET   default devkey / secret
#   ROOM            room name (default flexfec-test)
#   SUB_IDENTITY    participant identity (default flexfec-sub)
#   RUST_LOG        log filter (default info)
#   LOG_DIR         where to write logs (default scripts/flexfec/logs)
#   FEC_ONLY        if set to 1, only print FEC + frame-latency lines to the console
#
# Frame latency: when the publisher runs with --attach-timestamp/--attach-frame-id, the
# subscriber emits "Subscriber frame latency: ..." lines (capture->receive->decode). The
# receive_to_decode value is the jitter-buffer wait, which is where FEC-ON and RTX-only
# diverge; capture_to_decode is the end-to-end glass-to-decode latency.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
RUST_SDKS_DIR="${RUST_SDKS_DIR:-$REPO_ROOT/../rust-sdks5}"
LOG_DIR="${LOG_DIR:-$SCRIPT_DIR/logs}"

if [[ ! -d "$RUST_SDKS_DIR" ]]; then
  echo "error: rust-sdks checkout not found at '$RUST_SDKS_DIR' (set RUST_SDKS_DIR)" >&2
  exit 1
fi

export LIVEKIT_URL="${LIVEKIT_URL:-ws://127.0.0.1:7880}"
export LIVEKIT_API_KEY="${LIVEKIT_API_KEY:-devkey}"
export LIVEKIT_API_SECRET="${LIVEKIT_API_SECRET:-secret}"
export RUST_LOG="${RUST_LOG:-info}"
ROOM="${ROOM:-flexfec-test}"
SUB_IDENTITY="${SUB_IDENTITY:-flexfec-sub}"

mkdir -p "$LOG_DIR"
echo "==> Subscriber -> $LIVEKIT_URL room=$ROOM identity=$SUB_IDENTITY"
echo "    log: $LOG_DIR/subscriber.log   (grep 'Video FEC' for FEC, 'frame latency' for latency)"

cd "$RUST_SDKS_DIR"
set -o pipefail
if [[ "${FEC_ONLY:-0}" == "1" ]]; then
  cargo run -p local_video --features desktop --bin subscriber -- \
    --room-name "$ROOM" \
    --identity "$SUB_IDENTITY" \
    "$@" 2>&1 | tee "$LOG_DIR/subscriber.log" | grep --line-buffered -iE 'fec|recover|frame latency' || true
else
  cargo run -p local_video --features desktop --bin subscriber -- \
    --room-name "$ROOM" \
    --identity "$SUB_IDENTITY" \
    "$@" 2>&1 | tee "$LOG_DIR/subscriber.log"
fi
