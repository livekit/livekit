# How MoQ maps to LiveKit

A native MoQ publisher and a WebRTC viewer can share one LiveKit camera publication. The room gives the camera a track SID. The MoQ publisher sends encoded frames under that SID, the SFU packetizes them as RTP, and the viewer’s SDK attaches a playable track to the same publication. Room membership and track metadata travel separately over LiveKit signaling.

### Two paths through the same room

Both participants open a LiveKit signaling WebSocket. On the media path, the publisher opens native QUIC and the viewer uses a WebRTC peer connection. The SFU joins these paths at the room’s media track. Either participant can publish or subscribe. The arrows below follow a single camera from left to right.

```text
MoQ publisher             Room-owning SFU                 WebRTC viewer
    |--- WebSocket ----> room and publication state <--- WebSocket ---|
    |--- QUIC / LOC ---> media receiver and downtrack ---- SRTP ----->|
```

### Which identifier follows the track?

| Identifier | Scope and use |
| --- | --- |
| Participant SID, e.g. `PA_camera` | Names the publisher in signaling. On MoQ, each peer sends requests in its own namespace, such as `("livekit", "PA_reader")`. |
| Track SID, e.g. `TR_camera` | Identifies the room publication. Signaling, the SFU media track and the catalog’s `livekit.sid` use this value. |
| MoQ track alias, group and object ID | The alias routes subgroup streams or object datagrams to the corresponding track on this attachment. Group and object IDs locate a sample or fragment within that MoQ track. |
| RTP SSRC, timestamp and sequence number | Identify an RTP source, sampling instant and packet on a WebRTC leg. The SFU generates or rewrites them as needed for delivery. |

The examples use illustrative IDs and the native profile in these working trees, based on SFU `c5cfc84b`, Rust SDK `eb74c3d7` and shared protocol `a609e7a53312`.

Implementation sources · [Connection guide](moq-connection-guide.md) · [MoQ endpoint](../pkg/moq/endpoint.go) · [Participant attachment](../pkg/rtc/participant_moq.go)

## What maps onto MoQ?

The LiveKit profile uses three IETF drafts. MOQT defines how to publish, request and deliver objects. MSF describes the available media renditions. LOC describes the encoded media inside an object. LiveKit supplies the room identities, permissions and naming conventions that bind those objects to a room.

| LiveKit operation | MoQ concept and LiveKit-specific meaning |
| --- | --- |
| Attach to an admitted participant | MOQT SETUP establishes the MoQ session. The LiveKit WebSocket join supplies the endpoint, participant SID, generation and ticket. The SFU interprets those values in the SETUP path and binds the session to the existing participant. |
| Publish a camera or microphone | MOQT PUBLISH offers a named track and REQUEST_OK accepts the request. LiveKit assigns the SID in the WebSocket reply to `AddTrack`. The profile uses it in the upload name, e.g. `up/TR_camera/vp8`. |
| Discover and subscribe to a rendition | MSF describes each rendition’s name, role, codec and packaging. A viewer sends MOQT SUBSCRIBE and gets a track alias in SUBSCRIBE_OK. LiveKit auto-subscription or `UpdateSubscription` supplies room-level interest, which the SFU filters by permission. The `livekit.sid` and `livekit.publisherSid` extensions link a rendition to room state. |
| Deliver a video frame or Opus packet | A MOQT alias identifies the track. Group and object IDs locate the sample within it. LOC carries the encoded sample, timing, and optional video frame marking or audio level. A video keyframe starts a group in this profile. |
| Deliver an application DataTrack | MOQT OBJECT_DATAGRAM carries the payload. Each publication gets its own alias on each MoQ hop. LiveKit signaling creates the track and maps subscriber handles to SIDs. LiveKit defines how datagram chunks form a packet and how packets form an application frame. |

### What an independent player can do

After a LiveKit signaling join and attachment, a player can choose an advertised VP8 rendition by its MSF role and codec, subscribe by name, and read the keyframe using LOC. The repository’s independent subscriber test does this without reading the LiveKit catalog extensions. To show the publisher’s display name, current mute state or other people in the room, the player also needs the LiveKit signaling feed.

Implementation sources · [MOQT transport draft 23](https://www.ietf.org/archive/id/draft-ietf-moq-transport-23.html) · [MSF catalog draft 02](https://www.ietf.org/archive/id/draft-ietf-moq-msf-02.html) · [LOC media draft 05](https://www.ietf.org/archive/id/draft-ietf-moq-loc-05.html) · [Catalog extension](../pkg/moq/media/catalog.go) · [Independent subscriber](../../rust-sdks2/livekit/tests/moq_test.rs)

## Which events need LiveKit signaling?

The SFU sends LiveKit protocol messages on the participant’s WebSocket. The SDK applies those messages to its room model and emits typed callbacks. An independent client can implement the same signaling protocol and expose its own callbacks. A client reading only MoQ objects has a view of its subscribed tracks.

| LiveKit WebSocket message | What the application learns |
| --- | --- |
| `join`, `update` (ParticipantUpdate), `leave` | The join snapshot supplies the roster. Participant updates report peers joining, becoming active, leaving, or changing names, metadata, attributes or grants. A leave message addresses the receiving client’s own session. |
| `room_update` | Room-wide state and metadata. The SDK compares old and new values to emit `RoomMetadataChanged` and `RoomUpdated`. |
| `update` with media or DataTrack info | Remote track publication, removal and mute state, or logical DataTrack publication and removal. Disappearance from a viewer’s catalog alone is ambiguous because revoking access also removes the rendition. |
| `track_subscribed` | The **publisher** learns that its first visible viewer subscribed and receives `LocalTrackSubscribed`. The **viewer** gets its own MoQ request result, then a local `TrackSubscribed` callback when the SDK attaches playable media. |
| `speakers_changed`, `connection_quality` | The SFU’s active-speaker selection and participant quality ratings. LOC audio level and QUIC path measurements can supply inputs. The room’s decisions reach each viewer over signaling. |
| `refresh_token`, `reconnect` | Refreshed credentials and signaling resume information. The SDK reports room connection state as it completes or retries recovery. |

### Events the SDK constructs locally

`TrackSubscribed` combines a decoder track from MoQ with a remote publication from signaling. `TrackSubscriptionFailed` can come from a rejected rendition or a local decoder error. `Reconnecting` reflects the room engine entering recovery. These callbacks have no one-to-one wire message. DataTrack frames are read from the subscribed stream.

Legacy `DataReceived`, chat, RPC and text/byte streams use LiveKit’s WebRTC data-packet path. The current native MoQ profile does not deliver those features through signaling or its DataTrack objects.

Implementation sources · [Signaling protobuf](../../rust-sdks2/livekit-protocol/protocol/protobufs/livekit_rtc.proto) · [Server signals](../pkg/rtc/participant_signal.go) · [Rust room dispatcher](../../rust-sdks2/livekit/src/room/mod.rs) · [Rendition outcomes](../../rust-sdks2/livekit/src/rtc_engine/moq.rs)

## From room join to QUIC attachment

The Rust application makes one `Room::connect` call with `RoomTransport::Moq`, the signaling URL and its room token. On the server, enable the MoQ endpoint on the room-owning node and advertise a UDP address that reaches its shared listener.

```yaml
moq:
  enabled: true
  endpoint: moqt://node-1.example.com:7883/moq
webtransport:
  port: 7883
  tls_cert_file: /etc/livekit/tls/fullchain.pem
  tls_key_file: /etc/livekit/tls/key.pem
```

1. **Admit the participant.** The SDK joins over WebSocket at `/rtc?transport=moq`. Signaling can enter through another node. The owner creates one room participant, configures its transport manager for signaling only, and returns the room snapshot plus `JoinResponse.moq`.
2. **Bind the QUIC connection.** The SDK checks the participant SID and profile, connects to the advertised host using TLS 1.3 with ALPN `moqt-23`, and exchanges MOQT SETUP. Its request path carries the room name, participant SID, generation and a single-use attachment ticket. The owner validates the ticket and attaches the socket to that participant.
3. **Open the permitted requests.** The SDK opens the media catalog at join and opens a request for each DataTrack after signaling identifies the publication or subscription.

| Participant grant | MoQ requests |
| --- | --- |
| `can_publish_data` | After `PublishDataTrackResponse`, send `PUBLISH dt/up/<SID>` with a new alias and wait for `REQUEST_OK`. |
| `can_subscribe` | At join, `SUBSCRIBE catalog`. After the handle map arrives, `SUBSCRIBE dt/down/<SID>` for each track. Each accepted subscription has its own alias. |

The WebSocket stays open for room updates and publication control. A signaling resume can issue fresh attachment credentials while a healthy QUIC session continues to carry frames.

Implementation sources · [SFU configuration](../config-sample.yaml) · [Admission](../pkg/service/roommanager.go) · [Attachment ticket](../pkg/rtc/participant_moq.go) · [Rust connection](../../rust-sdks2/livekit/src/rtc_engine/moq.rs)

## MoQ publisher to WebRTC viewer

Suppose Alice publishes VP8 video from a native client and Bob views it in a browser. Bob receives an ordinary WebRTC video track. The conversion happens on the room-owning SFU.

1. **Reserve the publication.** Alice sends `AddTrack` over WebSocket. The publisher’s `TrackPublishedResponse` pairs the client’s correlation ID with `TrackInfo.sid = TR_camera`. Alice sends `PUBLISH up/TR_camera/vp8` in her own MoQ namespace. The SFU matches the SID to her pending track and checks the source grant, codec and attachment generation.
2. **Create the SFU track.** `StartMoQPublication` creates an `IngestReceiver`, attaches it to a LiveKit `MediaTrack` at layer zero and reports the publication to the room. Bob learns about Alice and her camera through participant signaling.
3. **Packetize the frame.** Alice sends one encoded access unit in a Low Overhead Container (LOC) object. Video groups start with a keyframe at object zero. The ingest receiver parses timing and frame marking and splits the codec payload to fit RTP packets.
4. **Attach at the viewer.** The SFU’s WebRTC downtrack sends SRTP over Bob’s negotiated peer connection. His browser exposes a `MediaStreamTrack` through its RTP receiver. The LiveKit JS SDK associates the receiver with the remote participant and publication, wraps it as a `RemoteVideoTrack`, and emits `RoomEvent.TrackSubscribed`.

### What happens to one frame?

| MoQ input | RTP representation at ingest |
| --- | --- |
| One VP8 object large enough for three RTP packets | Three successive sequence numbers, one SSRC and one timestamp. The final video packet carries the marker bit. Codec payload descriptors are added during packetization. |
| Next sample captured 20 ms later | At the 90 kHz video clock, its timestamp advances by 1,800 ticks. Sender-report state relates RTP time to capture time for synchronization. |
| One Opus object with an audio-level property | The receiver uses a 48 kHz RTP clock and carries the optional level and voice bit in an RTP header extension. The viewer gets an audio track. |

The SFU preserves the codec. The profile supports VP8, VP9, H.264, H.265, AV1 and Opus when the endpoints support them. In a browser, the usual LiveKit JS handler can attach the video to a DOM container.

```typescript
room.on(RoomEvent.TrackSubscribed, (track) => {
  if (track.kind === Track.Kind.Video) videoContainer.append(track.attach());
});
```

Implementation sources · [Publication binding](../pkg/rtc/participant_moq.go) · [RTP packetizer](../pkg/moq/media/ingest_receiver.go) · [Browser track attachment](../../client-sdk-js/src/room/Room.ts)

## WebRTC publisher to MoQ viewer

Now Bob publishes a WebRTC camera and Alice watches over MoQ. Bob’s RTP receiver on the SFU feeds two kinds of subscribers. WebRTC viewers use ordinary downtracks. For Alice, the SFU attaches an RTP tap that reassembles the encoded frames.

1. **Discover the rendition.** LiveKit signaling identifies Bob’s publication and records Alice’s subscription interest. Alice subscribes to `catalog` in her own namespace. Each changed MoQ Streaming Format (MSF) catalog is a full JSON snapshot of eligible renditions. The SFU checks grants, interest and publication permissions, and filters unsupported codecs and encrypted tracks.
2. **Subscribe by name.** Alice reads the catalog entry shown below and subscribes to its exact name in her namespace. `SUBSCRIBE_OK` assigns the alias that routes incoming subgroup streams to this subscription.
3. **Reassemble and send.** The SFU tap reads spatial layer zero, reorders RTP packets and removes codec transport headers. Each complete video frame becomes one LOC object. A keyframe starts a new group. Delta frames take consecutive object IDs. Each downstream object travels on its own subgroup stream. Each Opus packet starts a group of its own.
4. **Build the SDK track.** The Rust SDK initializes a decoder and a native libwebrtc audio or video source. It creates a `MediaStreamTrack` from that source and matches it to the signaled `RemoteTrackPublication` by publisher SID and track SID. The SDK wraps the result as a `RemoteAudioTrack` or `RemoteVideoTrack`. A MoQ-backed track has no RTP transceiver on this receiving leg.

A video entry in Alice’s catalog can look like this. The namespace on the request identifies Alice, while the catalog extension identifies Bob’s publication.

```json
{"name":"TR_camera/vp8", "role":"video", "codec":"vp8",
 "isLive":true, "packaging":"loc", "packagingMetadata":"streamMapping=object",
 "livekit":{"sid":"TR_camera", "publisherSid":"PA_bob"}}
```

### Track attachment and first frame

`TrackSubscribed` means the SDK attached a track to the publication. Applications can start a renderer or open a frame stream at this point. To measure time to first picture, wait for a decoded frame. After packet loss the SFU tap can request a new keyframe, and the MoQ decoder waits for a decodable sequence.

Signaling and the catalog arrive on independent connections. If a decoder track arrives first, the SDK holds it under the two SIDs and retries after participant metadata arrives. Catalog withdrawal detaches the playable track. The publication record can persist while permission or subscription interest changes.

Implementation sources · [Catalog and forwarding](../pkg/moq/media_track.go) · [RTP tap](../pkg/moq/rtp_tap.go) · [Rust decoder](../../rust-sdks2/livekit/src/rtc_engine/moq/media.rs) · [Remote track attachment](../../rust-sdks2/livekit/src/room/participant/remote_participant.rs)

## DataTracks across both transports

A DataTrack is a named room publication carrying application frames. Consider a track called `telemetry` with SID `DTR_telemetry`. Its publisher happens to use handle 7 and one subscriber is assigned handle 42. Both wire handles resolve to the same publication.

| Stage | What carries the identity and payload |
| --- | --- |
| Publish and discover | `PublishDataTrackRequest` names the track and supplies publisher handle 7. The SFU returns `DataTrackInfo` with the SID. Other clients learn the logical publication from participant signaling and can receive `DataTrackPublished`. |
| Upload over MoQ | Publish `dt/up/DTR_telemetry`. Each packet with handle 7 gets its own group on this track. Each 1024-byte chunk travels in one QUIC DATAGRAM. The last chunk can be shorter and marks END_OF_GROUP. The SFU forwards each complete packet. |
| Route in the room | The SFU checks that handle 7 belongs to the publication bound to the alias. Its DataDownTrack rewrites the packet with the recipient’s handle. WebSocket signaling maps handle 42 to the track SID and publisher. |
| Deliver over WebRTC | The SFU sends each rewritten packet as a message on the subscriber’s SCTP data channel labeled `_data_track`. This unordered channel uses zero retransmissions. The client routes by handle and depacketizes the frame. |
| Reverse the direction | A WebRTC publisher sends the same LiveKit packet format on `_data_track`. The SFU sends each rewritten packet as a datagram group on `dt/down/DTR_telemetry`. A missing chunk discards that packet. The SDK delivers later complete packets independently. |

### What the app reads

The Rust application calls `RemoteDataTrack::subscribe()`. The SDK signals its interest through `UpdateDataSubscription` and returns a stream of reassembled frames. In the example, the application gets one payload for the two packets. `DataTrackPublished` and `DataTrackUnpublished` describe lifecycle changes. Legacy data messages, built-in chat, RPC and text/byte streams use a separate WebRTC data-packet path that this MoQ profile does not carry.

MoQ participants always have DataTracks enabled. Publishing requires the data grant, and receiving requires an authorized subscription. A WebRTC leg also uses the server’s WebRTC DataTrack setting.

The alias identifies a logical flow of datagrams. Each track has its own queue. Tracks share the connection’s congestion control and currently use the same default publisher priority of 128.

Implementation sources · [MoQ DataTrack framing](../pkg/moq/datatrack.go) · [Handle rewriting](../pkg/rtc/datadowntrack.go) · [Subscriber handle map](../pkg/rtc/subscriptionmanager.go) · [Rust DataTrack stream](../../rust-sdks2/livekit-datatrack/src/remote/mod.rs)

## How a room event reaches the app

Suppose the room metadata changes from `lobby` to `live` while Alice is watching Bob. Alice receives the metadata event even if Bob’s camera is idle. The update travels over her signaling WebSocket. The Rust SDK creates the public event after applying the message to its local room model.

1. **The room owner records the change.** `Room.SetMetadata` updates the room protobuf and marks it dirty. The room’s update worker calls `sendRoomUpdate`, which builds a snapshot and sends it to each ready participant.
2. **Signaling delivers the snapshot.** `ParticipantImpl.SendRoomUpdate` writes a `SignalResponse.room_update` to Alice’s signaller. If her WebSocket terminates on a different node, the signal relay carries the response from the owner to that ingress. The RTC service drains her response source and calls `WriteResponse` on her WebSocket.
3. **The SDK applies it.** The Rust signaling reader decodes the response. The RTC session emits `SessionEvent::RoomUpdate`, the engine forwards it, and the room’s `handle_room_update` compares the new fields with its cached room.
4. **The app receives typed events.** With the metadata changed, the dispatcher emits `RoomMetadataChanged` with the old and new values and `RoomUpdated` with the updated room information. The receiver returned by `Room::connect` yields these as Rust `RoomEvent` values.

```text
Owner SFU         SignalResponse.room_update { room.metadata = "live" }
Signaling ingress WebSocket response to Alice
Rust room         cached metadata "lobby" becomes "live"
App receiver      RoomMetadataChanged, RoomUpdated
```

### How participant updates differ

At join, the SDK seeds its participant and media-publication maps from `JoinResponse`. The app receives the initial snapshot in `Connected { participants_with_tracks }`. Later, the room broadcasts `ParticipantInfo` updates. It can batch changes, filters hidden participants from other recipients and skips outdated participant versions. The SDK diffs each participant’s fields and track list to decide which public events to emit. A signaling message can produce several events.

### Who hears about a new camera?

The publishing client receives the reply to its own `AddTrack` request. Other clients discover the new `TrackInfo` in a participant update and can emit `TrackPublished`. A viewer emits `TrackSubscribed` when its SDK attaches the media. These are three different observations of the same track SID. The SFU sends protocol messages, and each client constructs the events appropriate to its role.

Implementation sources · [Room broadcasts](../pkg/rtc/room.go) · [Participant signaling](../pkg/rtc/participant_signal.go) · [WebSocket response pump](../pkg/service/rtcservice.go) · [Rust session](../../rust-sdks2/livekit/src/rtc_engine/rtc_session.rs) · [Room dispatcher](../../rust-sdks2/livekit/src/room/mod.rs)

## Where common events originate

The names below are Rust SDK events. The publisher and viewer can observe different events for the same camera. The trigger determines which participant sees the event and what an application can infer from it.

| What the application sees | Trigger and delivery |
| --- | --- |
| ParticipantConnected, ParticipantActive, ParticipantDisconnected | A signaled participant update adds a remote participant, marks it active or reports departure. A participant can be present before any media subscription exists. |
| ParticipantMetadataChanged, ParticipantAttributesChanged | The SDK compares signaled participant fields with its cache. Name and permission changes likewise have dedicated events. |
| TrackPublished, TrackUnpublished, TrackMuted, TrackUnmuted | A viewer reconciles the remote participant’s signaled track list and mute flags. A published or muted camera can be visible in room state while sending zero frames. |
| TrackSubscribed, TrackUnsubscribed, TrackSubscriptionFailed | The viewer attaches or detaches playable media. A rejected advertised MoQ rendition or decoder failure can produce a track-specific failure. An entry omitted from the catalog generates no MoQ request result. |
| LocalTrackSubscribed | The first visible media subscriber triggers the SFU’s once-per-publication callback. The publisher receives `SignalResponse.track_subscribed` and its SDK resolves the local track SID. A MoQ publisher can use this to begin capture before sending a frame. |
| ActiveSpeakersChanged | An Opus publisher can supply a LOC audio level and voice bit. The ingest receiver maps these into the RTP audio-level extension. The room computes speaker changes and sends scoped `speakers_changed` updates for the viewer and its subscribed publishers. |
| ConnectionQualityChanged | The SFU measures a MoQ path using smoothed RTT and recent outbound QUIC loss. It sends `connection_quality` updates for the viewer and subscribed publishers. The SDK maps each participant SID to a local or remote participant. |
| Reconnecting, Reconnected | The SDK engine reports signaling resume or full reconnect. A successful QUIC path migration can preserve the publication and subscription. A full reconnect can republish a local track with a new SID and emit `LocalTrackRepublished`. |

For speaker and quality updates, accepting a MoQ media subscription registers the viewer’s interest in that publisher with the SFU subscription manager. Closing the request releases that membership for the attachment generation.

Implementation sources · [Subscriber binding](../pkg/rtc/participant_moq.go) · [First-subscriber callback](../pkg/rtc/mediatrack.go) · [QUIC path scoring](../pkg/moq/path_quality.go) · [Rust event inventory](../../rust-sdks2/livekit/docs/moq-feature-parity.md)

## Observe a mixed-room subscription

This Rust excerpt runs inside an async function with a signaling URL and room token. Enable the SDK’s native `moq` and signaling TLS features. Keep the room handle alive while reading events. Here the two log lines distinguish discovering a camera from attaching its playable track.

```rust
use livekit::prelude::*;

let mut options = RoomOptions::default();
options.transport = RoomTransport::Moq;
let (_room, mut events) = Room::connect(url, token, options).await?;

while let Some(event) = events.recv().await {
    match event {
        RoomEvent::TrackPublished { participant, publication } => {
            println!("published {} {}", participant.sid(), publication.sid());
        }
        RoomEvent::TrackSubscribed { publication, .. } => {
            println!("attached {}", publication.sid());
        }
        RoomEvent::RoomMetadataChanged { old_metadata, metadata } => {
            println!("metadata {old_metadata} -> {metadata}");
        }
        RoomEvent::Disconnected { .. } => break,
        _ => {}
    }
}
```

### Follow the last observation

| Observation | What to inspect next |
| --- | --- |
| The publication exists but the MoQ viewer has no playable track. | Match publisher and track SIDs in signaling and catalog. Check subscription intent, grants, advertised codec and rendition request result. |
| The viewer received `TrackSubscribed` but has no picture. | Read the video frame stream. Check keyframe arrival, LOC timing and decoder output. |
| A logical DataTrack is visible but its stream is empty. | Check the logical subscription and handle map, then trace datagrams on `dt/down/<SID>` or `_data_track` for that subscriber. |
| Media continues but room updates have stopped. | Check the viewer’s signaling connection, relay and event loop. A healthy QUIC path can keep carrying frames during a WebSocket resume. |

The Rust SDK’s `datagram_data_tracks` test checks two simultaneous tracks and independent withdrawal. Its `room_transport_matrix` test exercises both mixed directions with decoded video, Opus and a fragmented 128 KiB DataTrack frame. The room lifecycle tests cover MoQ media arriving before its signaling metadata.

Implementation sources · [Mixed-room test](../../rust-sdks2/livekit/tests/moq_test.rs) · [Late-signaling tests](../../rust-sdks2/livekit/src/room/lifecycle_tests.rs) · [SFU test setup](../test/moq/README.md)
