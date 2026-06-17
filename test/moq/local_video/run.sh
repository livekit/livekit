#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LIVEKIT_DIR="$(cd "$SCRIPT_DIR/../../.." && pwd)"
WORKSPACE_DIR="$(cd "$LIVEKIT_DIR/.." && pwd)"

RUST_SDKS_DIR="${RUST_SDKS_DIR:-$WORKSPACE_DIR/rust-sdks}"
TMP_DIR="${TMP_DIR:-$(mktemp -d /private/tmp/livekit-moq-local-video.XXXXXX)}"

API_KEY="${API_KEY:-devkey}"
API_SECRET="${API_SECRET:-secret}"
ROOM="${ROOM:-moq-local-video}"
TRACK="${TRACK:-camera}"
PUBLISHER_IDENTITY="${PUBLISHER_IDENTITY:-moq-publisher}"
MOQ_IDENTITY="${MOQ_IDENTITY:-moq-client}"
WEBRTC_IDENTITY="${WEBRTC_IDENTITY:-webrtc-client}"

LIVEKIT_PORT="${LIVEKIT_PORT:-7880}"
LIVEKIT_RTC_TCP_PORT="${LIVEKIT_RTC_TCP_PORT:-7881}"
LIVEKIT_RTC_UDP_PORT="${LIVEKIT_RTC_UDP_PORT:-7882}"
MOQ_PORT="${MOQ_PORT:-7883}"
WEB_PORT="${WEB_PORT:-8899}"

WIDTH="${WIDTH:-640}"
HEIGHT="${HEIGHT:-480}"
FPS="${FPS:-60}"
FRAMES="${FRAMES:-30}"
OPEN="${OPEN:-0}"
PUBLISHER_READY_TIMEOUT="${PUBLISHER_READY_TIMEOUT:-120}"
PUBLISHER_RUST_LOG="${PUBLISHER_RUST_LOG:-info}"
PUBLISHER_ENCODER="${PUBLISHER_ENCODER:-software}"
PUBLISHER_LOW_LATENCY="${PUBLISHER_LOW_LATENCY:-${LOW_LATENCY:-1}}"
PUBLISHER_MIN_PLAYOUT_DELAY_MS="${PUBLISHER_MIN_PLAYOUT_DELAY_MS:-}"
PUBLISHER_MAX_PLAYOUT_DELAY_MS="${PUBLISHER_MAX_PLAYOUT_DELAY_MS:-}"

case "$PUBLISHER_LOW_LATENCY" in
  1|true|TRUE|yes|YES|on|ON)
    PUBLISHER_MIN_PLAYOUT_DELAY_MS="${PUBLISHER_MIN_PLAYOUT_DELAY_MS:-0}"
    PUBLISHER_MAX_PLAYOUT_DELAY_MS="${PUBLISHER_MAX_PLAYOUT_DELAY_MS:-1}"
    ;;
  0|false|FALSE|no|NO|off|OFF)
    ;;
  *)
    echo "invalid PUBLISHER_LOW_LATENCY=$PUBLISHER_LOW_LATENCY; expected 1/0, true/false, yes/no, or on/off" >&2
    exit 1
    ;;
esac

SERVER_PID=""
PUBLISHER_PID=""
WEB_PID=""

kill_tree() {
  local pid="$1"
  local child
  if [[ -z "$pid" ]] || ! kill -0 "$pid" >/dev/null 2>&1; then
    return
  fi
  while IFS= read -r child; do
    kill_tree "$child"
  done < <(pgrep -P "$pid" 2>/dev/null || true)
  kill "$pid" >/dev/null 2>&1
  wait "$pid" >/dev/null 2>&1
}

cleanup() {
  set +e
  for pid in "$PUBLISHER_PID" "$WEB_PID" "$SERVER_PID"; do
    kill_tree "$pid"
  done
}
trap cleanup EXIT INT TERM

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "missing required command: $1" >&2
    exit 1
  fi
}

wait_for_tcp() {
  local host="$1"
  local port="$2"
  local name="$3"
  local deadline=$((SECONDS + 30))
  until nc -z "$host" "$port" >/dev/null 2>&1; do
    if (( SECONDS >= deadline )); then
      echo "timed out waiting for $name on $host:$port" >&2
      exit 1
    fi
    sleep 0.2
  done
}

wait_for_log() {
  local file="$1"
  local pattern="$2"
  local name="$3"
  local pid="$4"
  local timeout="$5"
  local deadline=$((SECONDS + timeout))
  until grep -q "$pattern" "$file" >/dev/null 2>&1; do
    if ! kill -0 "$pid" >/dev/null 2>&1; then
      echo "$name exited before becoming ready; see $file" >&2
      tail -80 "$file" >&2
      exit 1
    fi
    if (( SECONDS >= deadline )); then
      echo "timed out waiting for $name readiness pattern '$pattern'; see $file" >&2
      tail -80 "$file" >&2
      exit 1
    fi
    sleep 0.5
  done
}

require_cmd cargo
require_cmd go
require_cmd nc
require_cmd openssl
require_cmd python3
require_cmd xxd

if [[ ! -d "$RUST_SDKS_DIR/examples/local_video" ]]; then
  echo "RUST_SDKS_DIR does not point at a rust-sdks checkout: $RUST_SDKS_DIR" >&2
  exit 1
fi

CERT_FILE="$TMP_DIR/moq-cert.pem"
KEY_FILE="$TMP_DIR/moq-key.pem"
OPENSSL_CONF="$TMP_DIR/openssl.cnf"
CONFIG_FILE="$TMP_DIR/livekit.yaml"

cat > "$OPENSSL_CONF" <<EOF_CONF
[req]
distinguished_name = req_distinguished_name
x509_extensions = v3_req
prompt = no

[req_distinguished_name]
CN = localhost

[v3_req]
subjectAltName = @alt_names

[alt_names]
DNS.1 = localhost
IP.1 = 127.0.0.1
EOF_CONF

openssl req \
  -x509 \
  -newkey ec \
  -pkeyopt ec_paramgen_curve:prime256v1 \
  -nodes \
  -days 1 \
  -keyout "$KEY_FILE" \
  -out "$CERT_FILE" \
  -config "$OPENSSL_CONF" >/dev/null 2>&1

CERT_HASH="$(openssl x509 -in "$CERT_FILE" -outform DER | openssl dgst -sha256 -binary | xxd -p -c 256)"

cat > "$CONFIG_FILE" <<EOF_CONFIG
development: true
port: $LIVEKIT_PORT
bind_addresses:
  - 127.0.0.1
rtc:
  use_external_ip: false
  tcp_port: $LIVEKIT_RTC_TCP_PORT
  udp_port: $LIVEKIT_RTC_UDP_PORT
keys:
  "$API_KEY": "$API_SECRET"
moq:
  enabled: true
  port: $MOQ_PORT
  bind_addresses:
    - 127.0.0.1
  path: /moq/v1
  cert_file: "$CERT_FILE"
  key_file: "$KEY_FILE"
  track_queue_size: 256
  cache_max_bytes: 2097152
  write_timeout: 2s
EOF_CONFIG

SERVER_BIN="${SERVER_BIN:-}"
if [[ -z "$SERVER_BIN" ]]; then
  SERVER_BIN="$TMP_DIR/livekit-server"
  echo "building livekit-server..."
  (cd "$LIVEKIT_DIR" && go build -o "$SERVER_BIN" ./cmd/server)
fi

TOKEN_OUTPUT="$("$SERVER_BIN" --config "$CONFIG_FILE" create-join-token --room "$ROOM" --identity "$MOQ_IDENTITY")"
TOKEN="$(printf "%s\n" "$TOKEN_OUTPUT" | sed -n "s/^Token: //p" | tail -1)"
if [[ -z "$TOKEN" ]]; then
  echo "failed to generate receiver token" >&2
  printf "%s\n" "$TOKEN_OUTPUT" >&2
  exit 1
fi

WEBRTC_TOKEN_OUTPUT="$("$SERVER_BIN" --config "$CONFIG_FILE" create-join-token --room "$ROOM" --identity "$WEBRTC_IDENTITY")"
WEBRTC_TOKEN="$(printf "%s\n" "$WEBRTC_TOKEN_OUTPUT" | sed -n "s/^Token: //p" | tail -1)"
if [[ -z "$WEBRTC_TOKEN" ]]; then
  echo "failed to generate WebRTC viewer token" >&2
  printf "%s\n" "$WEBRTC_TOKEN_OUTPUT" >&2
  exit 1
fi

echo "starting livekit-server..."
"$SERVER_BIN" --config "$CONFIG_FILE" > "$TMP_DIR/livekit.log" 2>&1 &
SERVER_PID="$!"
wait_for_tcp 127.0.0.1 "$LIVEKIT_PORT" "LiveKit"

publisher_cmd=(
  cargo run -p local_video -F desktop --bin publisher --
  --test-pattern
  --attach-timestamp
  --attach-frame-id
  --burn-timestamp
  --disable-dynacast
  --room-name "$ROOM"
  --identity "$PUBLISHER_IDENTITY"
  --codec h264
  --encoder "$PUBLISHER_ENCODER"
  --width "$WIDTH"
  --height "$HEIGHT"
  --fps "$FPS"
  --url "ws://127.0.0.1:$LIVEKIT_PORT"
  --api-key "$API_KEY"
  --api-secret "$API_SECRET"
)

if [[ -n "$PUBLISHER_MIN_PLAYOUT_DELAY_MS" ]]; then
  publisher_cmd+=(--min-playout-delay "$PUBLISHER_MIN_PLAYOUT_DELAY_MS")
fi
if [[ -n "$PUBLISHER_MAX_PLAYOUT_DELAY_MS" ]]; then
  publisher_cmd+=(--max-playout-delay "$PUBLISHER_MAX_PLAYOUT_DELAY_MS")
fi

if [[ -n "${PUBLISHER_ARGS:-}" ]]; then
  extra_args=($PUBLISHER_ARGS)
  publisher_cmd+=("${extra_args[@]}")
fi

echo "starting rust-sdks local_video publisher..."
(cd "$RUST_SDKS_DIR" && RUST_LOG="$PUBLISHER_RUST_LOG" "${publisher_cmd[@]}") > "$TMP_DIR/publisher.log" 2>&1 &
PUBLISHER_PID="$!"
wait_for_log "$TMP_DIR/livekit.log" "mediaTrack published" "publisher media track" "$PUBLISHER_PID" "$PUBLISHER_READY_TIMEOUT"

echo "starting receiver web server..."
python3 -m http.server "$WEB_PORT" --bind 127.0.0.1 --directory "$SCRIPT_DIR/receiver" > "$TMP_DIR/web.log" 2>&1 &
WEB_PID="$!"
wait_for_tcp 127.0.0.1 "$WEB_PORT" "receiver web server"

RECEIVER_URL="$(python3 - "$WEB_PORT" "$MOQ_PORT" "$LIVEKIT_PORT" "$TOKEN" "$WEBRTC_TOKEN" "$ROOM" "$TRACK" "$CERT_HASH" "$FRAMES" "$WEBRTC_IDENTITY" <<'PY'
import sys
from urllib.parse import urlencode

web_port, moq_port, livekit_port, token, webrtc_token, room, track, cert_hash, frames, webrtc_identity = sys.argv[1:]
query = urlencode({
    "url": f"https://127.0.0.1:{moq_port}/moq/v1",
    "token": token,
    "liveKitUrl": f"ws://127.0.0.1:{livekit_port}",
    "webrtcToken": webrtc_token,
    "webrtcIdentity": webrtc_identity,
    "room": room,
    "track": track,
    "certHash": cert_hash,
    "frames": frames,
})
print(f"http://127.0.0.1:{web_port}/?{query}")
PY
)"

MEET_URL="$(python3 - "$LIVEKIT_PORT" "$WEBRTC_TOKEN" <<'PY'
import sys
from urllib.parse import urlencode

livekit_port, token = sys.argv[1:]
query = urlencode({
    "liveKitUrl": f"ws://127.0.0.1:{livekit_port}",
    "token": token,
})
print(f"https://meet.livekit.io/custom/?{query}")
PY
)"

cat <<EOF_STATUS

MoQ local_video harness is running.

Receiver URL:
$RECEIVER_URL

LiveKit Meet URL:
$MEET_URL

WebRTC compare:
  URL:                ws://127.0.0.1:$LIVEKIT_PORT
  Room:               $ROOM
  Publisher identity: $PUBLISHER_IDENTITY
  MoQ identity:       $MOQ_IDENTITY
  WebRTC identity:    $WEBRTC_IDENTITY

Logs:
  LiveKit:   $TMP_DIR/livekit.log
  Publisher: $TMP_DIR/publisher.log
  Web:       $TMP_DIR/web.log

Press Ctrl-C to stop.
EOF_STATUS

if [[ "$OPEN" == "1" ]]; then
  if command -v open >/dev/null 2>&1; then
    open "$RECEIVER_URL"
  else
    echo "OPEN=1 requested, but the macOS 'open' command is unavailable" >&2
  fi
fi

while true; do
  if ! kill -0 "$SERVER_PID" >/dev/null 2>&1; then
    echo "livekit-server exited; see $TMP_DIR/livekit.log" >&2
    exit 1
  fi
  if ! kill -0 "$PUBLISHER_PID" >/dev/null 2>&1; then
    echo "publisher exited; see $TMP_DIR/publisher.log" >&2
    exit 1
  fi
  if ! kill -0 "$WEB_PID" >/dev/null 2>&1; then
    echo "receiver web server exited; see $TMP_DIR/web.log" >&2
    exit 1
  fi
  sleep 1
done
