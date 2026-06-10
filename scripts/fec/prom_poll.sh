#!/usr/bin/env bash
# Polls the SFU's prometheus endpoint once per second and appends the FEC and
# packet counters relevant to the FlexFEC test harness as tab-separated rows:
#   wall_us<TAB>metric{labels}<TAB>value
#
# Usage: ./prom_poll.sh <prometheus_port> <output_file>

set -u

PORT="${1:?prometheus port required}"
OUT="${2:?output file required}"

now_us() { python3 -c 'import time; print(int(time.time() * 1e6))'; }

while true; do
    TS=$(now_us)
    curl -s --max-time 2 "http://127.0.0.1:${PORT}/metrics" | \
        awk -v ts="$TS" '/^livekit_(flexfec_packet|nack|packet|packet_loss|packet_out_of_order)_total/ {
            value = $NF
            metric = $0
            sub(/ [^ ]*$/, "", metric)
            print ts "\t" metric "\t" value
        }' >> "$OUT"
    sleep 1
done
