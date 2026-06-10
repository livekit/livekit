#!/usr/bin/env bash
# Parameter sweep over the FlexFEC harness: runs run_fec_test.sh across a matrix
# of loss levels and FEC configurations, then aggregates all cells into a single
# comparison report (sweep_report.png + sweep_summary.csv + a markdown table).
#
# At each loss level it runs one baseline (no FEC) plus one FEC run per
# (rate, mask) combination, so every FEC point has a same-loss baseline to
# compare against. The server and example binaries are built once and reused.
#
# Usage:
#   ./sweep_fec.sh [options]
#
# Options (defaults in brackets):
#   --loss-mode {debug|shaped}  loss mechanism [debug]
#       debug  = uniform packet drop inside the SFU, no sudo, --loss-list is %
#       shaped = OS traffic shaping bursts (needs sudo), --loss-list is the
#                burst loss fraction, base loss/cadence fixed by --base-loss etc.
#   --loss-list "L1 L2 .."   loss levels to sweep [debug: "2 5 10 15"]
#   --fec-rate-list "R1 .."  publisher FEC protection rates (percent) ["20 30 50"]
#   --mask-list "M1 .."      FEC mask types: random and/or bursty ["bursty"]
#   --duration S             measurement seconds per cell [60]
#   --codec C                video codec [h264]
#   --base-loss F            shaped mode: continuous base loss [0.01]
#   --burst-every S          shaped mode: seconds between bursts [15]
#   --burst-len S            shaped mode: burst duration [3]
#   --out DIR                output directory [scripts/fec/out/sweep_<timestamp>]
#
# Example matrices:
#   # uniform-loss sweep (no sudo), 4 loss levels x 2 rates x 1 mask = 12 cells
#   ./sweep_fec.sh --loss-list "2 5 10 15" --fec-rate-list "20 50"
#
#   # shaped cellular-burst sweep, vary burst intensity and compare masks
#   sudo -v && ./sweep_fec.sh --loss-mode shaped --loss-list "0.15 0.30 0.50" \
#       --fec-rate-list "30" --mask-list "random bursty"
#
# Runtime ~= cells * (~20s setup + duration). The example above is ~12 * 80s.

set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
RUST_SDKS_DIR="${RUST_SDKS_DIR:-$(cd "$REPO_ROOT/.." && pwd)/rust-sdks}"
RUNNER="$SCRIPT_DIR/run_fec_test.sh"

LOSS_MODE="debug"
LOSS_LIST=""
FEC_RATE_LIST="20 30 50"
MASK_LIST="bursty"
DURATION=60
CODEC="h264"
BASE_LOSS=0.01
BURST_EVERY=15
BURST_LEN=3
OUT_DIR=""

log() { echo "[sweep] $*"; }
die() { echo "[sweep] ERROR: $*" >&2; exit 1; }

while [ $# -gt 0 ]; do
    case "$1" in
        --loss-mode) LOSS_MODE="$2"; shift 2 ;;
        --loss-list) LOSS_LIST="$2"; shift 2 ;;
        --fec-rate-list) FEC_RATE_LIST="$2"; shift 2 ;;
        --mask-list) MASK_LIST="$2"; shift 2 ;;
        --duration) DURATION="$2"; shift 2 ;;
        --codec) CODEC="$2"; shift 2 ;;
        --base-loss) BASE_LOSS="$2"; shift 2 ;;
        --burst-every) BURST_EVERY="$2"; shift 2 ;;
        --burst-len) BURST_LEN="$2"; shift 2 ;;
        --out) OUT_DIR="$2"; shift 2 ;;
        -h|--help) sed -n '2,38p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
        *) die "unknown argument: $1" ;;
    esac
done

case "$LOSS_MODE" in
    debug) : "${LOSS_LIST:=2 5 10 15}" ;;
    shaped) : "${LOSS_LIST:=0.15 0.30 0.50}" ;;
    *) die "--loss-mode must be debug or shaped" ;;
esac

command -v go >/dev/null || die "go not found"
command -v cargo >/dev/null || die "cargo not found"
python3 -c 'import matplotlib' 2>/dev/null || die "python3 with matplotlib required"
[ -d "$RUST_SDKS_DIR/examples/local_video" ] || die "rust-sdks not found at $RUST_SDKS_DIR"

if [ "$LOSS_MODE" = "shaped" ]; then
    sudo -v || die "shaped mode needs sudo (or use --loss-mode debug)"
    ( while true; do sudo -n true 2>/dev/null; sleep 60; done ) &
    SUDO_KEEPALIVE_PID=$!
    trap '[ -n "${SUDO_KEEPALIVE_PID:-}" ] && kill "$SUDO_KEEPALIVE_PID" 2>/dev/null' EXIT
fi

if [ -z "$OUT_DIR" ]; then
    OUT_DIR="$SCRIPT_DIR/out/sweep_$(date +%Y%m%d_%H%M%S)"
fi
mkdir -p "$OUT_DIR"
log "output directory: $OUT_DIR"

# build once, reuse across all cells
SERVER_BIN="$OUT_DIR/livekit-server"
log "building livekit-server (once)..."
(cd "$REPO_ROOT" && go build -o "$SERVER_BIN" ./cmd/server) || die "server build failed"
log "building local_video examples (once)..."
(cd "$RUST_SDKS_DIR" && cargo build --release -p local_video -F desktop --bin publisher --bin subscriber) \
    || die "example build failed"

MANIFEST="$OUT_DIR/manifest.tsv"
printf 'leaf\tmode\tloss\tfec_rate\tfec_mask\n' > "$MANIFEST"

run_cell() {
    # run_cell <mode> <cell_name> <extra args...>
    local mode="$1" cell="$2"; shift 2
    log "cell: $cell"
    "$RUNNER" --mode "$mode" --skip-build --server-bin "$SERVER_BIN" \
        --duration "$DURATION" --codec "$CODEC" --out "$OUT_DIR/$cell" "$@" \
        > "$OUT_DIR/$cell.log" 2>&1 || { log "WARNING: cell $cell failed, see $OUT_DIR/$cell.log"; return 1; }
}

loss_args() {
    local loss="$1"
    if [ "$LOSS_MODE" = "debug" ]; then
        echo "--debug-drop $loss"
    else
        echo "--base-loss $BASE_LOSS --burst-loss $loss --burst-every $BURST_EVERY --burst-len $BURST_LEN"
    fi
}

CELL_COUNT=0
for loss in $LOSS_LIST; do
    la=$(loss_args "$loss")

    base_cell="cell_loss${loss}_baseline"
    if run_cell baseline "$base_cell" $la; then
        printf '%s\t%s\t%s\t%s\t%s\n' "$base_cell/baseline" baseline "$loss" "-" "-" >> "$MANIFEST"
    fi
    CELL_COUNT=$((CELL_COUNT + 1))

    for rate in $FEC_RATE_LIST; do
        for mask in $MASK_LIST; do
            fec_cell="cell_loss${loss}_rate${rate}_${mask}"
            if run_cell fec "$fec_cell" --fec-rate "$rate" --fec-mask-type "$mask" $la; then
                printf '%s\t%s\t%s\t%s\t%s\n' "$fec_cell/fec" fec "$loss" "$rate" "$mask" >> "$MANIFEST"
            fi
            CELL_COUNT=$((CELL_COUNT + 1))
        done
    done
done

log "ran $CELL_COUNT cells, aggregating"
python3 "$SCRIPT_DIR/aggregate_fec.py" --sweep "$OUT_DIR" | tee "$OUT_DIR/sweep_summary.txt"
log "done. report in $OUT_DIR/sweep_report.png"
