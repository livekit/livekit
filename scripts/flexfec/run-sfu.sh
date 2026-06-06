#!/usr/bin/env bash
#
# Build and run the LiveKit SFU locally with FlexFEC-03 enabled.
#
# Uses `--dev` (devkey/secret API keys, loopback bind) together with the local
# FlexFEC config. Media is pinned to UDP 7882 so netem.sh can shape it.
#
# Usage:
#   scripts/flexfec/run-sfu.sh [extra go-run/server args...]
#
# Env overrides:
#   CONFIG          path to the SFU config (default: scripts/flexfec/flexfec-local.yaml)
#   Operator/subscriber side (SFU<->operator):
#     DOWNLINK_LOSS      fractional SFU->subscriber loss, e.g. 0.01 for 1% (unset = none)
#     DOWNLINK_DELAY_MS  one-way SFU->subscriber delay in ms (unset = none)
#     UPLINK_DELAY_MS    one-way subscriber->SFU feedback (NACK/RTCP) delay in ms
#   Robot/publisher side (robot<->SFU), e.g. a lossy 5G first hop:
#     PUB_LOSS           fractional publisher->SFU loss, e.g. 0.03 for 3% (unset = none)
#     PUB_DELAY_MS       one-way robot<->SFU delay in ms (inbound media + outbound NACK)
#
# These inject impairment in-SFU (pkg/sfu/downtrack_impair.go for the subscriber egress,
# pkg/sfu/buffer/buffer_impair.go for the publisher ingress), replacing netem.sh for the
# all-on-one-box setup where macOS pf/dummynet can't shape same-host UDP. Delays apply to
# retransmissions too; set the two delays equal for a symmetric link so a NACK->RTX
# recovery costs a full RTT -- the latency FlexFEC avoids by repairing inline.
#
# Teleop topology example (lossy 5G robot uplink, clean wired operator):
#   PUB_LOSS=0.03 PUB_DELAY_MS=40 scripts/flexfec/run-sfu.sh   # + flexfec.publisher: true
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
CONFIG="${CONFIG:-$SCRIPT_DIR/flexfec-local.yaml}"

cd "$REPO_ROOT"

if [[ -n "${DOWNLINK_LOSS:-}" ]]; then
  export LK_DOWNLINK_LOSS="$DOWNLINK_LOSS"
fi
if [[ -n "${DOWNLINK_DELAY_MS:-}" ]]; then
  export LK_DOWNLINK_DELAY_MS="$DOWNLINK_DELAY_MS"
fi
if [[ -n "${UPLINK_DELAY_MS:-}" ]]; then
  export LK_UPLINK_DELAY_MS="$UPLINK_DELAY_MS"
fi
if [[ -n "${PUB_LOSS:-}" ]]; then
  export LK_PUB_LOSS="$PUB_LOSS"
fi
if [[ -n "${PUB_DELAY_MS:-}" ]]; then
  export LK_PUB_DELAY_MS="$PUB_DELAY_MS"
fi

echo "==> Building & running LiveKit SFU with FlexFEC"
echo "    repo:   $REPO_ROOT"
echo "    config: $CONFIG"
echo "    url:    ws://127.0.0.1:7880   (devkey / secret)"
echo "    media:  udp/7882 on loopback"
echo "    operator downlink: loss=${LK_DOWNLINK_LOSS:-none} delay_ms=${LK_DOWNLINK_DELAY_MS:-none} nack_delay_ms=${LK_UPLINK_DELAY_MS:-none}"
echo "    robot uplink:      loss=${LK_PUB_LOSS:-none} delay_ms=${LK_PUB_DELAY_MS:-none}"
echo

exec go run ./cmd/server --dev --config "$CONFIG" "$@"
