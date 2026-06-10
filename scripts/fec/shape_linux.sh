#!/usr/bin/env bash
# Traffic shaper for the FlexFEC test harness (Linux, tc/netem).
#
# Applies packet loss to UDP traffic destined to the SFU's pinned media port on
# loopback, simulating a robot uplink over cellular: continuous low base loss
# (Gilbert-Elliott model for realistic loss correlation) plus periodic
# high-loss bursts. Burst on/off transitions are appended to an events file
# (wall-clock microseconds) so plots can shade the burst windows.
#
# Usage:
#   sudo ./shape_linux.sh start --port 7882 --base-loss 0.02 \
#       --burst-loss 0.25 --burst-every 15 --burst-len 3 --events events.csv
#   sudo ./shape_linux.sh stop
#
# `start` runs in the foreground until terminated, cleaning up on exit.
# `stop` force-cleans shaping state from a previous run.

set -u

DEV="lo"

PORT=7882
BASE_LOSS=0.02
BURST_LOSS=0.25
BURST_EVERY=15
BURST_LEN=3
EVENTS_FILE=""

log() { echo "[shape_linux] $*" >&2; }

now_us() { python3 -c 'import time; print(int(time.time() * 1e6))'; }

record_event() {
    if [ -n "$EVENTS_FILE" ]; then
        echo "$(now_us),$1" >> "$EVENTS_FILE"
    fi
}

pct() { python3 -c "print($1 * 100)"; }

apply_base_loss() {
    # Gilbert-Elliott: p = chance of entering the bad state, r = chance of
    # leaving it. p derived from the target average loss with r fixed at 30%
    # gives short correlated loss runs typical for radio links.
    local p
    p=$(pct "$BASE_LOSS")
    tc qdisc change dev $DEV parent 1:4 handle 40: netem loss gemodel "${p}%" 30%
}

cleanup() {
    trap - EXIT INT TERM
    log "cleaning up"
    tc qdisc del dev $DEV root 2>/dev/null
    record_event "shaper_stopped"
    log "done"
}

start() {
    trap cleanup EXIT INT TERM

    # 4-band prio qdisc: default TOS mapping never selects band 4, so only the
    # filtered SFU-bound UDP flow passes through the netem child
    tc qdisc add dev $DEV root handle 1: prio bands 4 priomap 1 2 2 2 1 2 0 0 1 1 1 1 1 1 1 1 || {
        log "failed to add root qdisc (already shaped? try '$0 stop')"
        exit 1
    }
    tc qdisc add dev $DEV parent 1:4 handle 40: netem loss gemodel "$(pct "$BASE_LOSS")%" 30%
    tc filter add dev $DEV parent 1: protocol ip prio 1 u32 \
        match ip protocol 17 0xff \
        match ip dport "$PORT" 0xffff \
        flowid 1:4

    log "shaping active: udp dport $PORT, base loss $BASE_LOSS (gemodel), burst $BURST_LOSS for ${BURST_LEN}s every ${BURST_EVERY}s"
    record_event "shaper_started base=$BASE_LOSS burst=$BURST_LOSS"

    # periodic burst loop
    while true; do
        sleep "$BURST_EVERY"
        tc qdisc change dev $DEV parent 1:4 handle 40: netem loss "$(pct "$BURST_LOSS")%"
        record_event "burst_on"
        log "burst on ($BURST_LOSS)"
        sleep "$BURST_LEN"
        apply_base_loss
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
