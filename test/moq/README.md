# Verify native MoQ interoperability

For SFU configuration, Rust SDK setup, architecture diagrams and the wire profile, see the [MoQ endpoint integration guide](../../docs/moq.md).

## Test the transport libraries

From the SFU checkout, run the standalone Go/Rust interoperability test. It starts a local QUIC listener with a temporary certificate, negotiates ALPN `moqt-23` and checks that a Rust client can publish a payload and receive its echo over a subscription. It requires Go and Cargo.

```sh
cd test/moq/interop
go test -race -count=1 ./...
```

## Test complete rooms

Start an SFU using the [guide's configuration](../../docs/moq.md#enable-the-sfu-endpoint). Give the test host access to both the signaling URL and the advertised UDP endpoint. For source builds before the shared protocol revision is published, use a Go workspace containing the SFU and matching protocol checkout, and initialize the SDK's pinned protocol submodule.

From the matching **Rust SDK checkout**, set `LIVEKIT_URL` to the test SFU's base URL and supply its `LIVEKIT_API_KEY` and `LIVEKIT_API_SECRET`. The suite uses those credentials to create test participants and change their permissions. For a private QUIC test CA, set `LIVEKIT_MOQ_TEST_CA_DER` to its DER certificate path. That override applies to the native MoQ connection in e2e builds.

```sh
cargo test --locked -p livekit \
  --features moq,__lk-e2e-test,rustls-tls-native-roots \
  --test moq_test -- --ignored
```

| Test | What a pass demonstrates |
| --- | --- |
| `datagram_data_tracks` | Two simultaneous DataTracks carry distinct payloads in both mixed directions and between two MoQ clients. A 128 KiB application frame spans multiple datagram groups. Unpublishing one track preserves delivery on the other. |
| `room_transport_matrix` | WebRTC → MoQ, MoQ → WebRTC and MoQ → MoQ each deliver an intact 128 KiB DataTrack frame, decoded Opus samples and at least five changing 1280×720 frames for each of H.264, H.265, VP8, VP9 and AV1. The test host needs working encoders and decoders for those codecs. |
| `first_subscriber_and_video_subscription_limit` | A MoQ publisher receives its first-subscriber event before capturing a frame. The viewer receives remote quality updates. See below to run the refusal-and-recovery case. |
| `independent_subscriber` | A moqtap client joins through LiveKit signaling, receives a refusal for a foreign namespace, discovers a rendition using standard catalog fields and verifies a VP8 keyframe. |
| `grant_video_and_data_after_join` | A MoQ participant granted subscribe and data permissions after joining receives video and publishes a DataTrack. Revoking and restoring subscribe permission produces unsubscribe and subscribe events and fresh decoded video on the same attachment. |
| `full_reconnect_republishes_video_and_data` | A full publisher restart restores delivery of video and a fragmented DataTrack frame. |
| `live_network_handoff_keeps_video_and_data_tracks` | After changing both clients' UDP sockets and resuming publisher signaling, the original subscriptions receive fresh decoded video and complete DataTrack frames. |

To exercise a real subscription refusal, start a separate test SFU with `limit.subscription_limit_video: 1` set in its YAML (under `limit`). Point `LIVEKIT_URL` at that instance and run the following command in the Rust SDK checkout. The second video must produce one track-specific failure, then subscribe after the first video is withdrawn on the same connection.

```sh
LIVEKIT_MOQ_TEST_SINGLE_VIDEO=1 cargo test --locked -p livekit \
  --features moq,__lk-e2e-test,rustls-tls-native-roots \
  --test moq_test first_subscriber_and_video_subscription_limit -- --ignored
```

## Add a network outage to the handoff test

1. Run a local IPv4 test SFU with `webtransport.port: 18883` and advertise `moq.endpoint: moqt://127.0.0.1:18884/moq`. Use a certificate valid for `127.0.0.1` and trusted by the test client.
2. From the SFU checkout, start the UDP relay in a separate terminal.

   ```sh
   python3 test/moq/handoff_relay.py --listen-port 18884 --sfu-port 18883
   ```

3. To exercise an IP change, set `LIVEKIT_MOQ_TEST_HANDOFF_IP` to a second local IPv4 address that can reach the relay. Leaving it unset exercises a port change. From the Rust SDK checkout, run only the handoff case against this SFU.

   ```sh
   cargo test --locked -p livekit \
     --features moq,__lk-e2e-test,rustls-tls-native-roots \
     --test moq_test -- --ignored --exact live_network_handoff_keeps_video_and_data_tracks
   ```

The relay starts a two-second bidirectional blackout on the first move, reports how many datagrams it dropped and fails if it sees a fresh QUIC v1 Initial after the handoff begins. A passing room test delivers new frames through the original subscriptions after the move and on the return path. Restart the relay before each run. This fixture simulates a network handoff on one machine.

## Compare publishing across two network uplinks

The [paired video handoff benchmark](HANDOFF.md) runs a standard WebRTC publisher and a native MoQ publisher through the same Linux interface loss and route change. A stable viewer reads IDs and capture timestamps from the decoded video to measure recovery, frame gaps, latency and loss.
