# How a native LiveKit MoQ connection starts

A Rust app calls `Room::connect` once. The SDK opens two connections for the same participant. A WebSocket to LiveKit signaling carries room membership, permissions, and track lifecycle messages. A QUIC connection to the SFU that owns the room carries encoded video and DataTrack packets. Signaling can enter the cluster through a different node.

## 1. Join the room over WebSocket

Enable the native Rust SDK's `moq` feature and a signaling TLS feature such as `rustls-tls-native-roots`. Give the SDK the normal signaling URL and a participant room token.

```rust
use livekit::prelude::{Room, RoomOptions, RoomTransport};

let mut options = RoomOptions::default();
options.transport = RoomTransport::Moq;
let (room, events) = Room::connect(signaling_url, participant_token, options).await?;
```

The SDK requests `/rtc?transport=moq` with its normal join parameters and sends the room JWT in the WebSocket upgrade's `Authorization: Bearer` header. The ingress forwards the choice to the room owner in `StartSession.use_moq_transport` (field 28). The owner checks its configuration and the participant's grants. It then admits the participant, creates a signaling-only transport manager, and sends the room, participant, and attachment details in the join response.

```text
Rust client          Signaling ingress             Room-owning SFU
    |-- WSS /rtc + JWT ----->|                            |
    |                        |-- StartSession ----------->|
    |<-------- JoinResponse via signaling ingress --------|
    |                                                     |
    |<========== QUIC + TLS 1.3 directly to owner =======>|
    |<========== MOQT SETUP in each direction ===========>|
    |-- grant-permitted PUBLISH / SUBSCRIBE ------------->|
    |<--------- request results and track aliases --------|
```

## 2. Read the attachment description

`JoinResponse.moq` (field 16) contains a `MoQTransportInfo`. Read it alongside the room and participant from that same response.

| Field | How the client or server uses it |
| --- | --- |
| `endpoint` | Native QUIC address, such as `moqt://node-1.example.com:7883/moq`. It includes an explicit UDP port. |
| `participant_sid` | Identifies the participant the socket will serve. The SDK checks it against the joined participant's SID. |
| `profile` | Identifies the supported wire format, `livekit-moq-2;moqt=23`. |
| `namespace_prefix` | Paired with the participant SID to name tracks. This implementation expects `livekit`. |
| `generation` | Identifies the ticket issuance. A newer issuance supersedes an outstanding older ticket. |
| `attachment_token` | Single-use bearer credential for this attachment. The SFU stores its digest and consumes it on success. |
| `expires_at` | Ticket deadline in Unix seconds. The current SFU issues tickets for 30 seconds and enforces expiry. |

The room JWT authorizes room admission. The attachment ticket allows a QUIC connection to claim the participant that signaling already admitted. Ticket expiry limits the time available to attach. A successfully attached connection can continue after that deadline.

## 3. Establish QUIC and bind it to the participant

The client resolves the advertised host, opens UDP, and completes a QUIC handshake with TLS 1.3. Rustls checks the server certificate for that host using platform trust. TLS negotiates the application protocol `moqt-23`, which directs the SFU's shared UDP listener to its native MoQ handler. The same listener can dispatch `h3` to HTTP/3.

Each peer sends a MOQT `SETUP` message on its own unidirectional control stream. The client includes the following options. Values shown here are illustrative.

```text
AUTHORITY  node-1.example.com:7883
PATH       /moq?room=demo&sid=PA_reader&generation=1&ticket=<REDACTED>
```

Use `room.name` for `room` and URL-encode each query value. The ticket-bearing path is a credential and should be redacted from logs. The owner resolves the room and SID locally, checks the generation, expiry and ticket, then binds the session to the participant. The server can send its own `SETUP` before finishing these checks. It serves application requests after validation. The server's five-second MOQT setup timeout and the ticket's 30-second validity are separate limits.

## 4. Open the initial MoQ requests

The SDK uses the participant's permissions to open long-lived requests. Track names below use that participant's own two-element namespace, for example `("livekit", "PA_reader")`.

| Grant | When the request opens | Success response |
| --- | --- | --- |
| `can_publish_data` | After the publish response, `PUBLISH dt/up/<SID>` with a client-selected alias | `REQUEST_OK` |
| `can_subscribe` | At join, `SUBSCRIBE catalog`. After signaling assigns a handle, `SUBSCRIBE dt/down/<SID>` | Each `SUBSCRIBE_OK` supplies an alias |

Each request has a bidirectional request stream. Catalogs and media use unidirectional subgroup streams. DataTracks use QUIC DATAGRAMs. Once attachment and the required initial requests succeed, the SDK can finish connecting the room. An empty catalog is valid. The first decoded frame is a later milestone.

## 5. Follow one camera frame

1. The publisher sends `AddTrack` over WebSocket. LiveKit returns a track SID, for example `TR_camera`.
2. The publisher sends `PUBLISH up/TR_camera/vp8` in its own namespace and waits for `REQUEST_OK`. It sends each coded frame as one Low Overhead Container (LOC) object with a timestamp in microseconds and frame marking. A keyframe begins a group at object zero. Delta frames follow with consecutive IDs.
3. The viewer receives a MOQT Streaming Format (MSF) catalog snapshot. It contains the renditions the viewer is permitted and has signaled interest to receive. An entry might name `TR_camera/vp8` and identify its publisher as `PA_camera`.
4. The viewer subscribes to that exact rendition name in **the viewer's own namespace**. The SFU returns `SUBSCRIBE_OK` with an alias and sends one object per downstream subgroup stream. The decoder starts with a keyframe. The SDK associates the decoded track with the signaled publication and emits `TrackSubscribed`.

For a WebRTC peer, the SFU packetizes encoded samples into RTP or reassembles RTP into samples, depending on direction. The codec stays the same. The video profile supports a single rendition of H.264, H.265, VP8, VP9 or AV1 when both endpoints support it. An independent MoQ viewer can use standard catalog and LOC fields after joining through LiveKit signaling.

## 6. Follow one application DataTrack frame

The publisher creates a logical track such as `telemetry` over WebSocket using a unique, nonzero publisher handle. LiveKit returns the publication info and track SID. The receiving app observes `DataTrackPublished` and calls `subscribe()`. Signaling supplies the mapping from incoming subscriber handles to the corresponding track SIDs.

```text
Publisher app    try_push(payload) on telemetry
dt/up/<SID>      one group per LiveKit packet, one datagram per chunk
                 group 0: object 0, object 1 (END_OF_GROUP)
SFU              reassembles the packet and rewrites the subscriber handle
dt/down/<SID>    sends the rewritten packet on the subscriber's track alias
Viewer app       depacketizes complete packets into application frames
```

The **track SID** identifies the room publication. The **handle** identifies that track in LiveKit packets on this hop. The **alias** identifies its MoQ datagrams on this connection. Both directions use up to 64 chunks of 1024 bytes per packet. A missing chunk can cost an application frame. Later complete groups continue immediately, including on the same track. QUIC does not retransmit the datagrams. A successful `try_push` reports local acceptance. Each track has its own alias and queue. The default publisher priority is 128. Call `room.set_moq_data_track_priority("telemetry", 16)` to give telemetry precedence over other pending frames at the default priority.

## 7. Handoff and recovery

| Event | What happens |
| --- | --- |
| Wi-Fi changes to cellular while QUIC is alive | QUIC validates the replacement path to the same room owner. The SDK monitors the selected route every 500 ms and can rebind the existing endpoint. Publications, subscriptions and track SIDs survive a successful migration. Delivery can pause during validation or retransmission. |
| Only WebSocket signaling reconnects | `ReconnectResponse.moq` (field 5) supplies fresh attachment credentials. Issuing them leaves the healthy active QUIC attachment in use. |
| The QUIC session closes | The SDK performs a full reconnect, obtains a new ticket, repeats TLS and MOQT setup, and reopens requests. Republished media can receive new track SIDs. |

A successful live migration uses the existing connection. A full reconnect uses an ordinary handshake in this implementation. Neither flow requires 0-RTT republishing.

## Configure and verify

DataTracks are always enabled for MoQ participants. Publishing requires the participant's data grant.

```yaml
moq:
  enabled: true
  endpoint: moqt://node-1.example.com:7883/moq
webtransport:
  port: 7883
  tls_cert_file: /etc/livekit/tls/fullchain.pem
  tls_key_file: /etc/livekit/tls/key.pem
```

Restart the SFU after applying the configuration. Clients must reach the advertised UDP port on the owning node, including after a source IP change. To verify a real join, set `LIVEKIT_URL`, `LIVEKIT_API_KEY`, and `LIVEKIT_API_SECRET` for the configured SFU, then run the independent subscriber test from the matching Rust SDK checkout.

```sh
cargo test --locked -p livekit \
  --features moq,__lk-e2e-test,rustls-tls-native-roots \
  --test moq_test independent_subscriber -- --ignored
```

If join fails with HTTP 412 for unavailable MoQ, check the owner node's configuration. If TLS fails after join, check UDP routing, certificate identity and ALPN. If SETUP reports an invalid attachment, check the ticket, room, SID, generation and destination. If setup succeeds but video is absent, check the reader's subscription intent, catalog and keyframe. For missing DataTrack payloads, check the data grant, logical subscription and handle mapping.

The implementation uses the shared LiveKit protocol revision `a609e7a53312`. The endpoint, attachment checks, and test commands are in the SFU repository. [Endpoint implementation](../pkg/moq/endpoint.go) · [Ticket lifecycle](../pkg/rtc/participant_moq.go) · [Interoperability tests](../test/moq/README.md)
