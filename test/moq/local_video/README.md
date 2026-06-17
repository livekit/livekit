# local_video MoQ Harness

This harness runs a local LiveKit server with the experimental MoQ/WebTransport
listener enabled, publishes H264 from the Rust SDK `examples/local_video`
publisher, and receives the stream in a browser with the `@moq/net` MoQ client
next to a LiveKit JS SDK WebRTC subscriber for the same room and track.

The current receiver validates transport compatibility and timing:

- WebTransport connect time
- first MoQ object time
- received frame/object count
- total payload bytes
- Annex-B H264 NAL types, including whether an IDR was seen
- WebCodecs H264 decode and canvas render count
- rendered canvas pixel validation to catch blank or flat output
- rendered-frame motion validation for the burned timestamp overlay
- packet-trailer timing metadata over a companion `<track>.timing` MoQ track
- subscriber-style timing overlay with exposure, receive, decode, paint, and
  e2e latency calculations
- side-by-side LiveKit JS SDK WebRTC rendering of the same publisher video
- WebRTC connect, first-frame, render, visual, motion, and browser-exposed
  video-frame timing metadata
- WebRTC packet-trailer extraction through the LiveKit JS SDK packet trailer
  worker, with frame ID and user timestamp lookup via `TimeSyncUpdate`

The MoQ object payload is one Annex-B H264 access unit per MoQ group/frame.
The browser receiver configures WebCodecs from the first SPS, decodes each
access unit, draws decoded frames into a canvas, and validates that nonblank,
moving video is rendered. The WebRTC pane attaches the remote video track with
`livekit-client` and samples the `<video>` element for the same visual and
motion checks. Chromium may expose WebRTC capture and receive timestamps through
`requestVideoFrameCallback`. Packet-trailer frame IDs and user timestamps are
decoded through `Room({ packetTrailer: { worker } })` and
`RemoteVideoTrack.lookupFrameMetadata({ rtpTimestamp })`. The receiver serves a
same-origin `packet-trailer-worker.js` shim because browsers block cross-origin
worker entrypoints; it imports the SDK worker module from esm.sh. The worker URL
can be overridden with the `packetTrailerWorkerUrl` receiver query parameter.

## Run

```bash
./test/moq/local_video/run.sh
```

Then open the printed receiver URL in a Chromium-based browser. Set `OPEN=1` to
open it automatically on macOS. The receiver URL includes both the MoQ token and
a separate WebRTC viewer token, so the page connects to the same room twice and
keeps both videos playing after the initial validation frame count is reached.

The runner defaults to:

- LiveKit HTTP/WebSocket: `127.0.0.1:7880`
- LiveKit RTC TCP/UDP: `127.0.0.1:7881` / `127.0.0.1:7882`
- MoQ WebTransport: `https://127.0.0.1:7883/moq/v1`
- receiver page: `http://127.0.0.1:8899`
- WebRTC compare URL: `ws://127.0.0.1:7880`
- Rust SDK checkout: `../rust-sdks`
- room: `moq-local-video`
- track: `camera`
- publisher: `--test-pattern --attach-timestamp --attach-frame-id --burn-timestamp --disable-dynacast --min-playout-delay 0 --max-playout-delay 1 --codec h264 --encoder software --width 640 --height 480 --fps 60`

`--disable-dynacast` keeps the Rust SDK publisher encoding continuously for
this MoQ proof. This checkout does not currently handle LiveKit
`SubscribedQualityUpdate` on the publisher side, so a pure MoQ subscriber cannot
wake dynacast by itself yet.

Useful overrides:

```bash
RUST_SDKS_DIR=/path/to/rust-sdks ROOM=my-room FRAMES=120 OPEN=1 ./test/moq/local_video/run.sh
WIDTH=1280 HEIGHT=720 FPS=60 PUBLISHER_ARGS="--max-bitrate 2500000" ./test/moq/local_video/run.sh
PUBLISHER_ENCODER=auto ./test/moq/local_video/run.sh
LOW_LATENCY=0 ./test/moq/local_video/run.sh
PUBLISHER_MIN_PLAYOUT_DELAY_MS=0 PUBLISHER_MAX_PLAYOUT_DELAY_MS=50 ./test/moq/local_video/run.sh
PUBLISHER_RUST_LOG=debug ./test/moq/local_video/run.sh
SERVER_BIN=/path/to/livekit-server ./test/moq/local_video/run.sh
```

Low-latency publisher mode is enabled by default and passes
`--min-playout-delay 0 --max-playout-delay 1`. Set `LOW_LATENCY=0` or
`PUBLISHER_LOW_LATENCY=0` to omit those flags, or set
`PUBLISHER_MIN_PLAYOUT_DELAY_MS` / `PUBLISHER_MAX_PLAYOUT_DELAY_MS` to pass
custom publisher playout-delay values.

The receiver uses `@moq/net@0.1.5` from esm.sh and negotiates `moq-lite-04`.
