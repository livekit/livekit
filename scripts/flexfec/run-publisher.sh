#!/usr/bin/env bash
#
# Run the rust-sdks local_video publisher with FlexFEC publishing enabled.
#
# Connects to the local dev SFU (run-sfu.sh) and publishes camera video with a
# FlexFEC-03 repair stream (--flex-fec). Output is teed to a log file.
#
# Usage:
#   scripts/flexfec/run-publisher.sh [extra publisher args...]
#
# Env overrides:
#   RUST_SDKS_DIR   path to the rust-sdks5 checkout (default: ../rust-sdks5)
#   LIVEKIT_URL     default ws://127.0.0.1:7880
#   LIVEKIT_API_KEY / LIVEKIT_API_SECRET   default devkey / secret
#   ROOM            room name (default flexfec-test)
#   PUB_IDENTITY    participant identity (default flexfec-pub)
#   RUST_LOG        log filter (default info)
#   LOG_DIR         where to write logs (default scripts/flexfec/logs)
#   TEST_PATTERN    1 = SMPTE test pattern (no camera); 0 = use camera (default 1)
#   ATTACH_META     1 = attach per-frame timestamp + frame id so the subscriber can log
#                   end-to-end frame latency; 0 = off (default 1)
#   MIN_PLAYOUT_DELAY / MAX_PLAYOUT_DELAY   ms; when set, recreates the room with a
#                   subscriber playout-delay cap (passed through to the publisher)
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
PUB_IDENTITY="${PUB_IDENTITY:-flexfec-pub}"

PUB_ARGS=(--flex-fec --room-name "$ROOM" --identity "$PUB_IDENTITY")
if [[ "${TEST_PATTERN:-1}" == "1" ]]; then
  # TEST_PATTERN_ANIMATE=1 -> animated (scrolling) pattern for a realistic motion bitrate.
  PUB_ARGS+=(--test-pattern "${TEST_PATTERN_ANIMATE:-0}")
fi
if [[ "${ATTACH_META:-1}" == "1" ]]; then
  PUB_ARGS+=(--attach-timestamp --attach-frame-id)
fi
if [[ -n "${MIN_PLAYOUT_DELAY:-}" ]]; then
  PUB_ARGS+=(--min-playout-delay "$MIN_PLAYOUT_DELAY")
fi
if [[ -n "${MAX_PLAYOUT_DELAY:-}" ]]; then
  PUB_ARGS+=(--max-playout-delay "$MAX_PLAYOUT_DELAY")
fi

mkdir -p "$LOG_DIR"
echo "==> Publisher -> $LIVEKIT_URL room=$ROOM identity=$PUB_IDENTITY (FlexFEC on)"
echo "    args: ${PUB_ARGS[*]}"
echo "    log:  $LOG_DIR/publisher.log"

cd "$RUST_SDKS_DIR"
set -o pipefail
cargo run -p local_video --features desktop --bin publisher -- \
  "${PUB_ARGS[@]}" \
  "$@" 2>&1 | tee "$LOG_DIR/publisher.log"
