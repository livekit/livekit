#!/usr/bin/env bash
#
# Linux network-namespace + netem harness for the asymmetric teleop FlexFEC test.
#
# Topology (models a robot on a lossy 5G uplink talking to a wired operator):
#
#     robot netns (publisher)            root netns (SFU + subscriber/operator)
#     10.200.0.2  veth-robot  <=========>  veth-host 10.200.0.1   [SFU :7880/:7882]
#                              netem here                          [subscriber -> 10.200.0.1]
#
# Only the robot<->SFU leg traverses the veth pair, so netem on the veth shapes the
# robot uplink/downlink ONLY. The subscriber (operator) sits in the root netns and reaches
# the SFU at 10.200.0.1 locally, so its link stays clean -- exactly the teleop case where
# the robot is on 5G and the operator is on a stable hardline.
#
# Loss is applied to the robot->SFU (uplink) direction; delay is applied symmetrically so a
# NACK->RTX recovery costs a full RTT (the latency publisher-side FlexFEC avoids by letting
# the SFU repair the stream inline).
#
# Usage:
#   sudo scripts/flexfec/netns.sh up                 # create ns + veth (no impairment)
#   sudo scripts/flexfec/netns.sh shape <loss%> <owd_ms>   # apply/replace netem
#   sudo scripts/flexfec/netns.sh clear              # remove netem (clean link)
#   sudo scripts/flexfec/netns.sh status             # show qdiscs + addrs
#   sudo scripts/flexfec/netns.sh down               # tear everything down
#   scripts/flexfec/netns.sh exec <cmd...>           # run cmd inside robot netns (as $SUDO_USER)
#
# Example:
#   sudo scripts/flexfec/netns.sh up
#   sudo scripts/flexfec/netns.sh shape 3 20         # 3% uplink loss, 20ms each way (40ms RTT)
set -euo pipefail

NS=robot
VETH_HOST=veth-host
VETH_ROBOT=veth-robot
HOST_IP=10.200.0.1
ROBOT_IP=10.200.0.2
PREFIX=24

# Privileged commands are run via passwordless sudo for ip/tc only (see the sudoers drop-in
# in the harness docs). The script itself does NOT need to run as root.
if [[ "$(id -u)" == "0" ]]; then
  SUDO=""
else
  SUDO="sudo -n"
fi

cmd_up() {
  # Idempotent: tear down any prior instance first.
  $SUDO ip netns del "$NS" 2>/dev/null || true
  $SUDO ip link del "$VETH_HOST" 2>/dev/null || true

  $SUDO ip netns add "$NS"
  $SUDO ip link add "$VETH_HOST" type veth peer name "$VETH_ROBOT"
  $SUDO ip link set "$VETH_ROBOT" netns "$NS"

  $SUDO ip addr add "$HOST_IP/$PREFIX" dev "$VETH_HOST"
  $SUDO ip link set "$VETH_HOST" up

  $SUDO ip netns exec "$NS" ip addr add "$ROBOT_IP/$PREFIX" dev "$VETH_ROBOT"
  $SUDO ip netns exec "$NS" ip link set "$VETH_ROBOT" up
  $SUDO ip netns exec "$NS" ip link set lo up

  echo "up: $NS netns ready"
  echo "  root: $VETH_HOST $HOST_IP/$PREFIX   robot: $VETH_ROBOT $ROBOT_IP/$PREFIX"
  echo "  SFU should advertise $HOST_IP (use flexfec-linux.yaml); publisher connects to ws://$HOST_IP:7880"
}

# shape <loss_percent> <one_way_delay_ms> [jitter_ms] [corr_pct]
# When jitter_ms/corr_pct are given they feed netem's loss correlation + delay jitter to
# approximate bursty (e.g. 5G handoff) conditions rather than uniform random loss.
cmd_shape() {
  local loss="${1:?usage: shape <loss%> <owd_ms> [jitter_ms] [loss_corr%]}"
  local owd="${2:?usage: shape <loss%> <owd_ms> [jitter_ms] [loss_corr%]}"
  local jitter="${3:-0}"
  local corr="${4:-0}"

  local delayspec="delay ${owd}ms"
  [[ "$jitter" != "0" ]] && delayspec="delay ${owd}ms ${jitter}ms distribution normal"
  local lossspec="loss ${loss}%"
  [[ "$corr" != "0" ]] && lossspec="loss ${loss}% ${corr}%"

  # Uplink (robot -> SFU): loss + delay. Applied on robot-side egress.
  $SUDO ip netns exec "$NS" tc qdisc replace dev "$VETH_ROBOT" root netem $delayspec $lossspec
  # Downlink (SFU -> robot, carries NACK/RTCP/PLI): delay only, no loss (operator link is
  # clean, but the return path still costs propagation delay so RTX pays a full RTT).
  $SUDO tc qdisc replace dev "$VETH_HOST" root netem delay "${owd}ms"

  echo "shape: uplink ${lossspec} ${delayspec} ; downlink delay=${owd}ms (RTT≈$((owd*2))ms)"
}

cmd_clear() {
  $SUDO ip netns exec "$NS" tc qdisc del dev "$VETH_ROBOT" root 2>/dev/null || true
  $SUDO tc qdisc del dev "$VETH_HOST" root 2>/dev/null || true
  echo "clear: netem removed (clean link)"
}

cmd_status() {
  echo "=== root ns: $VETH_HOST ==="
  $SUDO ip addr show "$VETH_HOST" 2>/dev/null | sed -n '2,4p' || echo "  (absent)"
  $SUDO tc qdisc show dev "$VETH_HOST" 2>/dev/null || true
  echo "=== robot ns: $VETH_ROBOT ==="
  $SUDO ip netns exec "$NS" ip addr show "$VETH_ROBOT" 2>/dev/null | sed -n '2,4p' || echo "  (absent)"
  $SUDO ip netns exec "$NS" tc qdisc show dev "$VETH_ROBOT" 2>/dev/null || true
}

cmd_down() {
  $SUDO ip netns del "$NS" 2>/dev/null || true
  $SUDO ip link del "$VETH_HOST" 2>/dev/null || true
  echo "down: torn down"
}

# exec <cmd...> : run inside robot netns, dropping back to the invoking user. The outer
# `sudo ip netns exec` runs as root; the inner `sudo -u $user` drops privileges (root -> user
# needs no password), so the publisher runs as the normal user with its env intact.
cmd_exec() {
  local user="${SUDO_USER:-$USER}"
  exec $SUDO ip netns exec "$NS" sudo -u "$user" --preserve-env=LIVEKIT_URL,LIVEKIT_API_KEY,LIVEKIT_API_SECRET,RUST_LOG,ROOM,PUB_IDENTITY,PATH,HOME,CARGO_HOME,RUSTUP_HOME "$@"
}

action="${1:-}"
shift || true
case "$action" in
  up)     cmd_up "$@" ;;
  shape)  cmd_shape "$@" ;;
  clear)  cmd_clear "$@" ;;
  status) cmd_status "$@" ;;
  down)   cmd_down "$@" ;;
  exec)   cmd_exec "$@" ;;
  *) echo "usage: $0 {up|shape <loss%> <owd_ms>|clear|status|down|exec <cmd...>}" >&2; exit 2 ;;
esac
