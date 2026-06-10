#!/usr/bin/env bash
# End-to-end FlexFEC test harness: publisher (rust-sdks local_video) -> SFU,
# with traffic shaping on the publisher->SFU leg simulating a cellular uplink
# (continuous base loss + periodic loss bursts), collecting frame metadata at
# the subscriber and FEC/NACK/loss counters from the SFU, then plotting time
# series and printing a summary.
#
# Usage:
#   ./run_fec_test.sh --mode {fec|baseline|ab} [options]
#
# Modes:
#   fec       single run with --flex-fec on the publisher
#   baseline  single run without FlexFEC (NACK/RTX recovery only)
#   ab        baseline run followed by a fec run with the same loss profile,
#             producing a comparison report
#
# Options (defaults in brackets):
#   --duration S       measurement duration per run after media starts [120]
#   --out DIR          output directory [scripts/fec/out/<timestamp>]
#   --base-loss F      continuous loss fraction [0.02]
#   --burst-loss F     loss fraction inside bursts [0.1]
#   --burst-every S    seconds between burst starts [15]
#   --burst-len S      burst duration in seconds [1]
#   --fec-rate N       publisher FEC protection rate percent, 0 = adaptive [30]
#   --fec-mask-type T  random|bursty [bursty]
#   --codec C          video codec [h264]
#   --width/--height/--fps   test pattern format [1280/720/30]
#   --no-shaping       skip traffic shaping (sanity run)
#   --debug-drop PCT   drop PCT% of received packets inside the SFU instead of
#                      OS traffic shaping (uniform loss, no sudo required;
#                      implies --no-shaping)
#   --server-bin PATH  use this prebuilt livekit-server instead of building
#   --skip-build       skip go/cargo builds (requires --server-bin and
#                      pre-built example binaries); used by sweep_fec.sh
#
# Requires: go, cargo, python3 with matplotlib, curl, and sudo (for shaping).
# The rust-sdks checkout is located via RUST_SDKS_DIR [../rust-sdks].

set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
RUST_SDKS_DIR="${RUST_SDKS_DIR:-$(cd "$REPO_ROOT/.." && pwd)/rust-sdks}"

MODE=""
DURATION=120
OUT_DIR=""
BASE_LOSS=0.02
BURST_LOSS=0.1
BURST_EVERY=15
BURST_LEN=1
FEC_RATE=30
FEC_MASK_TYPE="bursty"
CODEC="h264"
WIDTH=1280
HEIGHT=720
FPS=30
SHAPING=1
DEBUG_DROP=0
SERVER_BIN=""
SKIP_BUILD=0

SIGNAL_PORT=7880
MEDIA_PORT=7882
PROM_PORT=6789
API_KEY="devkey"
API_SECRET="fec-test-secret-fec-test-secret-00"
ROOM_NAME="fec-test"

log() { echo "[fec-test] $*"; }
die() { echo "[fec-test] ERROR: $*" >&2; exit 1; }
now_us() { python3 -c 'import time; print(int(time.time() * 1e6))'; }

while [ $# -gt 0 ]; do
    case "$1" in
        --mode) MODE="$2"; shift 2 ;;
        --duration) DURATION="$2"; shift 2 ;;
        --out) OUT_DIR="$2"; shift 2 ;;
        --base-loss) BASE_LOSS="$2"; shift 2 ;;
        --burst-loss) BURST_LOSS="$2"; shift 2 ;;
        --burst-every) BURST_EVERY="$2"; shift 2 ;;
        --burst-len) BURST_LEN="$2"; shift 2 ;;
        --fec-rate) FEC_RATE="$2"; shift 2 ;;
        --fec-mask-type) FEC_MASK_TYPE="$2"; shift 2 ;;
        --codec) CODEC="$2"; shift 2 ;;
        --width) WIDTH="$2"; shift 2 ;;
        --height) HEIGHT="$2"; shift 2 ;;
        --fps) FPS="$2"; shift 2 ;;
        --no-shaping) SHAPING=0; shift ;;
        --debug-drop) DEBUG_DROP="$2"; SHAPING=0; shift 2 ;;
        --server-bin) SERVER_BIN="$2"; shift 2 ;;
        --skip-build) SKIP_BUILD=1; shift ;;
        -h|--help) sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
        *) die "unknown argument: $1" ;;
    esac
done

case "$MODE" in
    fec|baseline|ab) ;;
    *) die "--mode must be fec, baseline or ab" ;;
esac

case "$(uname -s)" in
    Darwin) SHAPER="$SCRIPT_DIR/shape_macos.sh"; LOOPBACK_IF="lo0" ;;
    Linux) SHAPER="$SCRIPT_DIR/shape_linux.sh"; LOOPBACK_IF="lo" ;;
    *) die "unsupported platform $(uname -s)" ;;
esac

# ---------- preflight ----------

command -v go >/dev/null || die "go not found"
command -v cargo >/dev/null || die "cargo not found"
command -v curl >/dev/null || die "curl not found"
python3 -c 'import matplotlib' 2>/dev/null || die "python3 with matplotlib required (pip3 install matplotlib)"
[ -d "$RUST_SDKS_DIR/examples/local_video" ] || die "rust-sdks not found at $RUST_SDKS_DIR (set RUST_SDKS_DIR)"

if [ "$SHAPING" = "1" ]; then
    log "shaping requires sudo, validating credentials..."
    sudo -v || die "sudo required for traffic shaping (or pass --no-shaping)"
    # keep the sudo timestamp alive for long runs
    ( while true; do sudo -n true 2>/dev/null; sleep 60; done ) &
    SUDO_KEEPALIVE_PID=$!
fi

if [ -z "$OUT_DIR" ]; then
    OUT_DIR="$SCRIPT_DIR/out/$(date +%Y%m%d_%H%M%S)"
fi
mkdir -p "$OUT_DIR"
log "output directory: $OUT_DIR"

# ---------- builds ----------

if [ "$SKIP_BUILD" = "1" ]; then
    [ -n "$SERVER_BIN" ] || die "--skip-build requires --server-bin"
    [ -x "$SERVER_BIN" ] || die "server binary not found at $SERVER_BIN"
    log "skipping builds, using prebuilt $SERVER_BIN"
else
    SERVER_BIN="${SERVER_BIN:-$OUT_DIR/livekit-server}"
    log "building livekit-server..."
    (cd "$REPO_ROOT" && go build -o "$SERVER_BIN" ./cmd/server) || die "server build failed"

    log "building local_video examples (release)..."
    (cd "$RUST_SDKS_DIR" && cargo build --release -p local_video -F desktop --bin publisher --bin subscriber) \
        || die "example build failed"
fi
PUBLISHER_BIN="$RUST_SDKS_DIR/target/release/publisher"
SUBSCRIBER_BIN="$RUST_SDKS_DIR/target/release/subscriber"
[ -x "$PUBLISHER_BIN" ] || die "publisher binary not found, run once without --skip-build first"
[ -x "$SUBSCRIBER_BIN" ] || die "subscriber binary not found, run once without --skip-build first"

# ---------- process management ----------

SERVER_PID=""
SUBSCRIBER_PID=""
PUBLISHER_PID=""
PROM_POLL_PID=""
SHAPER_PID=""

stop_pid() {
    local pid="$1" sig="${2:-TERM}"
    if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
        kill -"$sig" "$pid" 2>/dev/null
        for _ in 1 2 3 4 5 6 7 8 9 10; do
            kill -0 "$pid" 2>/dev/null || return 0
            sleep 0.5
        done
        kill -KILL "$pid" 2>/dev/null
    fi
}

stop_shaper() {
    if [ -n "$SHAPER_PID" ] && kill -0 "$SHAPER_PID" 2>/dev/null; then
        sudo kill -TERM "$SHAPER_PID" 2>/dev/null
        sleep 2
    fi
    SHAPER_PID=""
    sudo "$SHAPER" stop >/dev/null 2>&1 || true
}

cleanup() {
    trap - EXIT INT TERM
    log "cleaning up processes"
    [ "$SHAPING" = "1" ] && stop_shaper
    stop_pid "$PROM_POLL_PID"
    stop_pid "$PUBLISHER_PID" INT
    stop_pid "$SUBSCRIBER_PID" INT
    stop_pid "$SERVER_PID"
    [ -n "${SUDO_KEEPALIVE_PID:-}" ] && kill "$SUDO_KEEPALIVE_PID" 2>/dev/null
    exit "${1:-1}"
}
trap 'cleanup 1' INT TERM
trap 'cleanup $?' EXIT

# ---------- single run ----------

run_one() {
    local mode="$1"
    local run_dir="$OUT_DIR/$mode"
    mkdir -p "$run_dir"
    log "=== $mode run: ${DURATION}s ==="

    # media is pinned to the loopback interface: it keeps every packet on the
    # shaped path and, on macOS, avoids the application firewall silently
    # dropping inbound UDP for unsigned freshly-built binaries
    cat > "$run_dir/server.yaml" <<EOF
port: $SIGNAL_PORT
bind_addresses:
  - 127.0.0.1
rtc:
  udp_port: $MEDIA_PORT
  use_external_ip: false
  enable_loopback_candidate: true
  interfaces:
    includes:
      - $LOOPBACK_IF
  enable_flexfec: true
prometheus:
  port: $PROM_PORT
keys:
  $API_KEY: $API_SECRET
room:
  auto_create: true
logging:
  level: info
EOF

    log "starting livekit-server"
    LIVEKIT_DEBUG_RX_DROP_PCT="$DEBUG_DROP" \
        "$SERVER_BIN" --config "$run_dir/server.yaml" > "$run_dir/server.log" 2>&1 &
    SERVER_PID=$!

    for i in $(seq 1 40); do
        curl -s -o /dev/null --max-time 1 "http://127.0.0.1:$SIGNAL_PORT" && break
        kill -0 "$SERVER_PID" 2>/dev/null || die "server exited early, see $run_dir/server.log"
        [ "$i" = "40" ] && die "server did not become reachable"
        sleep 0.5
    done
    log "server is up (pid $SERVER_PID)"

    local conn_args="--url ws://127.0.0.1:$SIGNAL_PORT --api-key $API_KEY --api-secret $API_SECRET --room-name $ROOM_NAME"

    log "starting subscriber (headless)"
    RUST_LOG=info "$SUBSCRIBER_BIN" $conn_args \
        --identity fec-sub --headless --log-frames "$run_dir/frames.csv" \
        > "$run_dir/subscriber.log" 2>&1 &
    SUBSCRIBER_PID=$!
    sleep 2

    local fec_args=""
    if [ "$mode" = "fec" ]; then
        fec_args="--flex-fec --fec-mask-type $FEC_MASK_TYPE"
        if [ "$FEC_RATE" != "0" ]; then
            fec_args="$fec_args --fec-protection-rate $FEC_RATE"
        fi
    fi

    log "starting publisher (test pattern ${WIDTH}x${HEIGHT}@${FPS} $CODEC${fec_args:+,$fec_args})"
    RUST_LOG=info "$PUBLISHER_BIN" $conn_args \
        --identity fec-pub --test-pattern \
        --width "$WIDTH" --height "$HEIGHT" --fps "$FPS" --codec "$CODEC" \
        --attach-timestamp --attach-frame-id $fec_args \
        > "$run_dir/publisher.log" 2>&1 &
    PUBLISHER_PID=$!

    log "waiting for media to flow..."
    for i in $(seq 1 120); do
        if [ -f "$run_dir/frames.csv" ] && [ "$(wc -l < "$run_dir/frames.csv")" -gt 30 ]; then
            break
        fi
        kill -0 "$PUBLISHER_PID" 2>/dev/null || die "publisher exited early, see $run_dir/publisher.log"
        kill -0 "$SUBSCRIBER_PID" 2>/dev/null || die "subscriber exited early, see $run_dir/subscriber.log"
        [ "$i" = "120" ] && die "no frames received after 60s"
        sleep 0.5
    done

    local t0
    t0=$(now_us)
    log "media flowing, starting measurement (t0=$t0)"
    cat > "$run_dir/meta.env" <<EOF
MODE=$mode
T0_US=$t0
DURATION_S=$DURATION
BASE_LOSS=$BASE_LOSS
BURST_LOSS=$BURST_LOSS
BURST_EVERY=$BURST_EVERY
BURST_LEN=$BURST_LEN
SHAPING=$SHAPING
DEBUG_DROP=$DEBUG_DROP
FEC_RATE=$FEC_RATE
FEC_MASK_TYPE=$FEC_MASK_TYPE
CODEC=$CODEC
EOF

    "$SCRIPT_DIR/prom_poll.sh" "$PROM_PORT" "$run_dir/prom.tsv" &
    PROM_POLL_PID=$!

    if [ "$SHAPING" = "1" ]; then
        sudo "$SHAPER" start --port "$MEDIA_PORT" \
            --base-loss "$BASE_LOSS" --burst-loss "$BURST_LOSS" \
            --burst-every "$BURST_EVERY" --burst-len "$BURST_LEN" \
            --events "$run_dir/events.csv" \
            > "$run_dir/shaper.log" 2>&1 &
        SHAPER_PID=$!
        sleep 1
        kill -0 "$SHAPER_PID" 2>/dev/null || die "shaper failed to start, see $run_dir/shaper.log"
    fi

    sleep "$DURATION"

    log "measurement done, stopping"
    [ "$SHAPING" = "1" ] && stop_shaper
    echo "T1_US=$(now_us)" >> "$run_dir/meta.env"
    stop_pid "$PROM_POLL_PID"; PROM_POLL_PID=""
    stop_pid "$PUBLISHER_PID" INT; PUBLISHER_PID=""
    stop_pid "$SUBSCRIBER_PID" INT; SUBSCRIBER_PID=""
    # the final flexfec stats log line is emitted when the publisher's buffers close
    sleep 2
    grep -h "flexfec" "$run_dir/server.log" | tail -5 || true
    stop_pid "$SERVER_PID"; SERVER_PID=""
    sleep 1
}

# ---------- runs + report ----------

case "$MODE" in
    fec) run_one fec ;;
    baseline) run_one baseline ;;
    ab)
        run_one baseline
        sleep 3
        run_one fec
        ;;
esac

log "generating report"
if [ "$MODE" = "ab" ]; then
    python3 "$SCRIPT_DIR/plot_fec.py" --run "$OUT_DIR/baseline" --run "$OUT_DIR/fec" --out "$OUT_DIR" \
        | tee "$OUT_DIR/summary.txt"
else
    python3 "$SCRIPT_DIR/plot_fec.py" --run "$OUT_DIR/$MODE" --out "$OUT_DIR" \
        | tee "$OUT_DIR/summary.txt"
fi

log "done. results in $OUT_DIR"
