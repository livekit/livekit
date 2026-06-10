#!/usr/bin/env bash
# Traffic shaper for the FlexFEC test harness (macOS, dummynet via dnctl/pfctl).
#
# Applies packet loss to UDP traffic destined to the SFU's pinned media port on
# loopback, simulating a robot uplink over cellular: continuous low base loss
# plus periodic high-loss bursts. Burst on/off transitions are appended to an
# events file (wall-clock microseconds) so plots can shade the burst windows.
#
# Usage:
#   sudo ./shape_macos.sh start --port 7882 --base-loss 0.02 \
#       --burst-loss 0.25 --burst-every 15 --burst-len 3 --events events.csv
#   sudo ./shape_macos.sh stop
#
# `start` runs in the foreground until terminated, cleaning up on exit.
# `stop` force-cleans shaping state from a previous run.

set -u

ANCHOR="livekit_fec"
PIPE=1
STATE_DIR="${TMPDIR:-/tmp}/livekit_fec_shaper"

PORT=7882
BASE_LOSS=0.02
BURST_LOSS=0.25
BURST_EVERY=15
BURST_LEN=3
EVENTS_FILE=""

log() { echo "[shape_macos] $*" >&2; }

now_us() { python3 -c 'import time; print(int(time.time() * 1e6))'; }

record_event() {
    if [ -n "$EVENTS_FILE" ]; then
        echo "$(now_us),$1" >> "$EVENTS_FILE"
    fi
}

cleanup() {
    trap - EXIT INT TERM
    log "cleaning up"
    pfctl -a "$ANCHOR" -F all 2>/dev/null
    # restore the system ruleset, dropping our anchor attachment
    pfctl -f /etc/pf.conf 2>/dev/null
    dnctl -q flush 2>/dev/null
    if [ -f "$STATE_DIR/pf_token" ]; then
        pfctl -X "$(cat "$STATE_DIR/pf_token")" 2>/dev/null
        rm -f "$STATE_DIR/pf_token"
    fi
    record_event "shaper_stopped"
    log "done"
}

start() {
    mkdir -p "$STATE_DIR"

    if ! command -v dnctl >/dev/null; then
        log "ERROR: dnctl not found. dummynet is unavailable on this system,"
        log "consider Network Link Conditioner or running the test on Linux."
        exit 1
    fi

    trap cleanup EXIT INT TERM

    # configure the dummynet pipe with the base loss
    dnctl pipe $PIPE config plr "$BASE_LOSS" || { log "dnctl failed"; exit 1; }

    # enable pf, keeping the reference token for clean disable
    local token
    token=$(pfctl -E 2>&1 | awk '/Token/ {print $NF}')
    if [ -n "$token" ]; then
        echo "$token" > "$STATE_DIR/pf_token"
    fi

    # attach our dummynet anchor on top of the system ruleset
    pfctl -q -f - <<EOF
include "/etc/pf.conf"
dummynet-anchor "$ANCHOR"
anchor "$ANCHOR"
EOF

    # shape UDP packets addressed to the SFU media port (publisher -> SFU leg)
    echo "dummynet in quick proto udp from any to any port $PORT pipe $PIPE" | \
        pfctl -q -a "$ANCHOR" -f -

    log "shaping active: udp dport $PORT, base loss $BASE_LOSS, burst $BURST_LOSS for ${BURST_LEN}s every ${BURST_EVERY}s"
    record_event "shaper_started base=$BASE_LOSS burst=$BURST_LOSS"

    # periodic burst loop
    while true; do
        sleep "$BURST_EVERY"
        dnctl pipe $PIPE config plr "$BURST_LOSS"
        record_event "burst_on"
        log "burst on ($BURST_LOSS)"
        sleep "$BURST_LEN"
        dnctl pipe $PIPE config plr "$BASE_LOSS"
        record_event "burst_off"
        log "burst off ($BASE_LOSS)"
    done
}

CMD="${1:-}"
shift || true
while [ $# -gt 0 ]; do
    case "$1" in
        --port) PORT="$2"; shift 2 ;;
        --base-loss) BASE_LOSS="$2"; shift 2 ;;
        --burst-loss) BURST_LOSS="$2"; shift 2 ;;
        --burst-every) BURST_EVERY="$2"; shift 2 ;;
        --burst-len) BURST_LEN="$2"; shift 2 ;;
        --events) EVENTS_FILE="$2"; shift 2 ;;
        *) log "unknown argument: $1"; exit 1 ;;
    esac
done

if [ "$(id -u)" -ne 0 ]; then
    log "ERROR: must run as root (sudo)"
    exit 1
fi

case "$CMD" in
    start) start ;;
    stop) cleanup ;;
    *) echo "usage: $0 {start|stop} [--port N] [--base-loss F] [--burst-loss F] [--burst-every S] [--burst-len S] [--events FILE]" >&2; exit 1 ;;
esac
