#!/usr/bin/env bash
#
# Simulate packet loss on the SFU's media UDP port so FlexFEC recovery can be
# observed end-to-end. Works on macOS (dnctl/dummynet + pfctl) and Linux (tc netem).
#
# Because publisher, SFU and subscriber all run on one machine, media flows over the
# loopback interface. Loss is matched by the SFU UDP port (default 7882):
#   - "down" drops packets coming FROM the SFU port  -> SFU -> subscriber path
#            (exercises subscriber-side / downlink FEC generation+recovery)
#   - "up"   drops packets going TO the SFU port      -> publisher -> SFU path
#            (exercises publisher-side / uplink FEC reception+recovery)
#   - "both" drops in both directions
#
# Usage:
#   sudo scripts/flexfec/netem.sh start [loss_percent] [up|down|both] [delay_ms]  # uniform loss; default: 5 down 0
#   sudo scripts/flexfec/netem.sh burst [loss_percent] [burst_ms] [gap_ms] [up|down|both] [delay_ms]  # bursty loss
#   sudo scripts/flexfec/netem.sh stop
#   sudo scripts/flexfec/netem.sh status
#
# delay_ms adds one-way latency (so RTT ~= 2*delay_ms on loopback). Models the
# robot's access-network round trip; RTX recovery costs ~1 RTT of buffering.
#
# "burst" emulates handoff/fade-style correlated loss: it toggles the loss rate
# between loss_percent (for burst_ms) and 0 (for gap_ms) on a duty cycle, holding
# the delay constant. Average loss ~= loss_percent * burst_ms/(burst_ms+gap_ms).
# Run it in its own terminal; Ctrl-C stops and restores the network.
# This is where RTX (retransmit storms, each costing an RTT) and FlexFEC (fixed
# inline overhead) diverge most.
#
# Env overrides:
#   SFU_UDP_PORT  media port to shape (default 7882)
#   NETEM_IFACE   (Linux only) interface to shape (default lo)
set -euo pipefail

PORT="${SFU_UDP_PORT:-7882}"
ANCHOR="flexfec-loss"
STATE_DIR="${TMPDIR:-/tmp}/flexfec-netem"
PF_STATE="$STATE_DIR/pf.enabled"
OS="$(uname -s)"

require_root() {
  if [[ "$(id -u)" -ne 0 ]]; then
    echo "error: this command needs root; re-run with sudo" >&2
    exit 1
  fi
}

validate_dir() {
  case "$1" in
    up | down | both) ;;
    *)
      echo "error: direction must be up|down|both (got '$1')" >&2
      exit 1
      ;;
  esac
}

# ----------------------------------------------------------------------------- macOS

# Install the pf anchor + dummynet rules for the requested direction. Pipes are
# (re)configured separately so loss can be toggled live for burst mode.
macos_setup() {
  local dir="$1"

  mkdir -p "$STATE_DIR"

  # Remember whether pf was already enabled so `stop` can restore it.
  if [[ ! -f "$PF_STATE" ]]; then
    if pfctl -s info 2>/dev/null | grep -q 'Status: Enabled'; then
      echo enabled >"$PF_STATE"
    else
      echo disabled >"$PF_STATE"
    fi
  fi

  local rules=""
  case "$dir" in
    down | both) rules+="dummynet out proto udp from any port $PORT to any pipe 1"$'\n' ;;
  esac
  case "$dir" in
    up | both) rules+="dummynet out proto udp from any to any port $PORT pipe 2"$'\n' ;;
  esac

  # Re-load the system ruleset plus references to our named anchor (non-destructive:
  # existing /etc/pf.conf rules are preserved), then install our rules in the anchor.
  {
    cat /etc/pf.conf 2>/dev/null || true
    echo "dummynet-anchor \"$ANCHOR\""
    echo "anchor \"$ANCHOR\""
  } | pfctl -f - 2>/dev/null
  printf '%s' "$rules" | pfctl -a "$ANCHOR" -f - 2>/dev/null
  pfctl -e 2>/dev/null || true

  if pfctl -sa 2>/dev/null | grep -qi 'skip on lo'; then
    echo "warning: pf is configured to 'skip on lo'; loopback shaping may not apply" >&2
  fi
}

# macos_set_loss <loss_percent> <delay_ms>
macos_set_loss() {
  local plr
  plr="$(awk "BEGIN { printf \"%.4f\", $1 / 100 }")"
  dnctl pipe 1 config plr "$plr" delay "$2" >/dev/null
  dnctl pipe 2 config plr "$plr" delay "$2" >/dev/null
}

macos_start() {
  local loss="$1" dir="$2" delay="$3"
  macos_setup "$dir"
  macos_set_loss "$loss" "$delay"
  echo "==> macOS dummynet loss enabled: ${loss}% loss, ${delay}ms delay (${dir}) on udp/${PORT}"
}

macos_burst() {
  local loss="$1" burst_ms="$2" gap_ms="$3" dir="$4" delay="$5"
  local burst_s gap_s avg
  burst_s="$(awk "BEGIN { printf \"%.3f\", $burst_ms / 1000 }")"
  gap_s="$(awk "BEGIN { printf \"%.3f\", $gap_ms / 1000 }")"
  avg="$(awk "BEGIN { printf \"%.1f\", $loss * $burst_ms / ($burst_ms + $gap_ms) }")"

  macos_setup "$dir"
  macos_set_loss 0 "$delay"

  trap 'echo; echo "==> stopping burst"; macos_stop; exit 0' INT TERM
  echo "==> macOS bursty loss: ${loss}% for ${burst_ms}ms every $((burst_ms + gap_ms))ms (~${avg}% avg), ${delay}ms delay (${dir}) on udp/${PORT}"
  echo "    Ctrl-C to stop and restore the network."
  while true; do
    macos_set_loss "$loss" "$delay"
    printf '\r  [burst ON  %s%% loss]   ' "$loss"
    sleep "$burst_s"
    macos_set_loss 0 "$delay"
    printf '\r  [burst off  0%% loss]   ' "$loss"
    sleep "$gap_s"
  done
}

macos_stop() {
  dnctl -q flush 2>/dev/null || true
  printf '' | pfctl -a "$ANCHOR" -f - 2>/dev/null || true
  pfctl -f /etc/pf.conf 2>/dev/null || true
  if [[ -f "$PF_STATE" && "$(cat "$PF_STATE")" == "disabled" ]]; then
    pfctl -d 2>/dev/null || true
  fi
  rm -f "$PF_STATE"
  echo "==> macOS dummynet loss disabled"
}

macos_status() {
  echo "== dummynet pipes =="
  dnctl list 2>/dev/null || true
  echo "== pf anchor '$ANCHOR' =="
  pfctl -a "$ANCHOR" -s rules 2>/dev/null || true
  echo "== pf status =="
  pfctl -s info 2>/dev/null | head -1 || true
}

# ----------------------------------------------------------------------------- Linux

# linux_setup <dir> <delay_ms>: build prio qdisc + netem band (loss starts at 0)
# and funnel matching UDP packets into it.
linux_setup() {
  local dir="$1" delay="$2"
  local dev="${NETEM_IFACE:-lo}"

  local netem_args=(loss 0%)
  if [[ "$delay" -gt 0 ]]; then
    netem_args+=(delay "${delay}ms")
  fi

  tc qdisc del dev "$dev" root 2>/dev/null || true
  tc qdisc add dev "$dev" root handle 1: prio
  tc qdisc add dev "$dev" parent 1:3 handle 30: netem "${netem_args[@]}"

  if [[ "$dir" == "down" || "$dir" == "both" ]]; then
    tc filter add dev "$dev" parent 1:0 protocol ip prio 3 u32 \
      match ip protocol 17 0xff match ip sport "$PORT" 0xffff flowid 1:3
  fi
  if [[ "$dir" == "up" || "$dir" == "both" ]]; then
    tc filter add dev "$dev" parent 1:0 protocol ip prio 3 u32 \
      match ip protocol 17 0xff match ip dport "$PORT" 0xffff flowid 1:3
  fi
}

# linux_set_loss <loss_percent> <delay_ms>
linux_set_loss() {
  local dev="${NETEM_IFACE:-lo}"
  if [[ "$2" -gt 0 ]]; then
    tc qdisc change dev "$dev" parent 1:3 handle 30: netem loss "${1}%" delay "${2}ms"
  else
    tc qdisc change dev "$dev" parent 1:3 handle 30: netem loss "${1}%"
  fi
}

linux_start() {
  local loss="$1" dir="$2" delay="$3"
  local dev="${NETEM_IFACE:-lo}"
  linux_setup "$dir" "$delay"
  linux_set_loss "$loss" "$delay"
  echo "==> Linux netem enabled: ${loss}% loss, ${delay}ms delay (${dir}) on udp/${PORT} dev ${dev}"
}

linux_burst() {
  local loss="$1" burst_ms="$2" gap_ms="$3" dir="$4" delay="$5"
  local dev="${NETEM_IFACE:-lo}"
  local burst_s gap_s avg
  burst_s="$(awk "BEGIN { printf \"%.3f\", $burst_ms / 1000 }")"
  gap_s="$(awk "BEGIN { printf \"%.3f\", $gap_ms / 1000 }")"
  avg="$(awk "BEGIN { printf \"%.1f\", $loss * $burst_ms / ($burst_ms + $gap_ms) }")"

  linux_setup "$dir" "$delay"

  trap 'echo; echo "==> stopping burst"; linux_stop; exit 0' INT TERM
  echo "==> Linux bursty loss: ${loss}% for ${burst_ms}ms every $((burst_ms + gap_ms))ms (~${avg}% avg), ${delay}ms delay (${dir}) on udp/${PORT} dev ${dev}"
  echo "    Ctrl-C to stop and restore the network."
  while true; do
    linux_set_loss "$loss" "$delay"
    printf '\r  [burst ON  %s%% loss]   ' "$loss"
    sleep "$burst_s"
    linux_set_loss 0 "$delay"
    printf '\r  [burst off  0%% loss]   ' "$loss"
    sleep "$gap_s"
  done
}

linux_stop() {
  local dev="${NETEM_IFACE:-lo}"
  tc qdisc del dev "$dev" root 2>/dev/null || true
  echo "==> Linux netem loss disabled (dev ${dev})"
}

linux_status() {
  local dev="${NETEM_IFACE:-lo}"
  echo "== qdisc (dev ${dev}) =="
  tc qdisc show dev "$dev" || true
  echo "== filters (dev ${dev}) =="
  tc filter show dev "$dev" || true
}

# ----------------------------------------------------------------------------- dispatch

cmd="${1:-}"

case "$cmd" in
  start)
    require_root
    loss="${2:-5}"
    dir="${3:-down}"
    delay="${4:-0}"
    validate_dir "$dir"
    case "$OS" in
      Darwin) macos_start "$loss" "$dir" "$delay" ;;
      Linux) linux_start "$loss" "$dir" "$delay" ;;
      *) echo "error: unsupported OS '$OS'" >&2; exit 1 ;;
    esac
    ;;
  burst)
    require_root
    loss="${2:-40}"
    burst_ms="${3:-200}"
    gap_ms="${4:-2000}"
    dir="${5:-down}"
    delay="${6:-20}"
    validate_dir "$dir"
    case "$OS" in
      Darwin) macos_burst "$loss" "$burst_ms" "$gap_ms" "$dir" "$delay" ;;
      Linux) linux_burst "$loss" "$burst_ms" "$gap_ms" "$dir" "$delay" ;;
      *) echo "error: unsupported OS '$OS'" >&2; exit 1 ;;
    esac
    ;;
  stop)
    require_root
    case "$OS" in
      Darwin) macos_stop ;;
      Linux) linux_stop ;;
    esac
    ;;
  status)
    case "$OS" in
      Darwin) macos_status ;;
      Linux) linux_status ;;
    esac
    ;;
  *)
    echo "usage: sudo $0 {start [loss%] [up|down|both] [delay_ms]" >&2
    echo "               | burst [loss%] [burst_ms] [gap_ms] [up|down|both] [delay_ms]" >&2
    echo "               | stop | status}" >&2
    exit 1
    ;;
esac
