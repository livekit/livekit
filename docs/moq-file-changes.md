# MoQ file changes in the LiveKit SFU

This is the server-side review companion to the [MoQ integration guide](moq.md). The baseline is server commit `c5cfc84b`. File paths are relative to the repository root. The matching SDK inventory is `livekit/docs/moq-file-changes.md` in the Rust SDK checkout.

To follow a request, start with `rtcservice.go` and `roommanager.go` for WebSocket admission. Then read `participant_moq.go` and `pkg/moq/endpoint.go` together for the ticket exchange. After attachment, video follows `media_track.go` and its RTP adapters, while DataTracks use `datatrack.go`. Both repositories depend on LiveKit protocol revision `a609e7a53312`.

## Configuration, build and operator documentation

| File | Change | What changed |
| --- | --- | --- |
| [README.md](../README.md) | Modified | Adds the self-hosted MoQ integration guide to Documentation & Guides, giving operators an entry point from the repository root. |
| [config-sample.yaml](../config-sample.yaml) | Modified | Adds a commented example for `moq.enabled`, the advertised `moqt://host:port/moq` URL, the shared UDP listener and its TLS key pair. |
| [pkg/config/config.go](../pkg/config/config.go) | Modified | Exposes `MoQConfig` as the top-level `moq` YAML section and calls its validator with `WebTransport.Port` when constructing the server configuration. |
| [pkg/config/moq.go](../pkg/config/moq.go) | New | Defines `Enabled` and `Endpoint`. Validation is conditional on enablement. It checks the listener port range and requires a `moqt` URL with an explicit host, port and exact `/moq` path. Credentials, query strings and fragments are rejected. |
| [pkg/config/moq_test.go](../pkg/config/moq_test.go) | New | Covers the default-off setting, an endpoint configured without enablement, valid IPv6 configuration and invalid listener ports or advertised URLs. The tests exercise both YAML loading and direct validation. |
| [go.mod](../go.mod) | Modified | Raises the source-build minimum from Go 1.26.0 to 1.27.4. Adds Trickster v2.2.2, advances LiveKit protocol from v1.54.1 to the pseudo-version ending in `a609e7a53312`, and updates the dependency graph. The existing quic-go version is v0.64.1. |
| [go.sum](../go.sum) | Modified | Updates module and manifest checksums for the new protocol revision, Trickster and the resolved dependency graph. This is the checksum companion to the root `go.mod`. |
| [docs/moq.md](../docs/moq.md) | New | Adds deployment YAML, Rust client setup, mixed-room and join diagrams, a third-party subscriber walkthrough, the DataTrack wire contract, handoff behavior and troubleshooting. Links to this file inventory. |
| [docs/moq-file-changes.md](../docs/moq-file-changes.md) | New | Records the server worktree delta against the baseline below and identifies the matching Rust SDK inventory. This is the file you are reading. |

## Signaling, admission and QUIC dispatch

| File | Change | What changed |
| --- | --- | --- |
| [pkg/routing/interfaces.go](../pkg/routing/interfaces.go) | Modified | Adds `UseMoQTransport` to `ParticipantInit`, exposes it in structured logging when set, and round-trips it through the inter-node `StartSession` message. The room owner receives the choice made at WebSocket ingress. |
| [pkg/routing/messagechannel.go](../pkg/routing/messagechannel.go) | Modified | Adds a terminal error to `MessageChannel`, stores it under the channel lock during close, and exposes `Err()` for the consumer. Ordinary `Close()` uses the same path with a nil error. |
| [pkg/routing/signal.go](../pkg/routing/signal.go) | Modified | Passes the relay reader’s termination error to the response channel. The signaling service can then recover an admission rejection issued by another node. |
| [pkg/service/rtcservice.go](../pkg/service/rtcservice.go) | Modified | Parses `transport` on the `/rtc` join, rejects unknown values and incompatible WebRTC join modes, and forwards the transport choice. Propagates a failed precondition when the initial relay closes and stops retrying that admission failure. |
| [pkg/service/roommanager.go](../pkg/service/roommanager.go) | Modified | Makes the room owner authoritative for enablement. Rejects a disabled endpoint before room creation or identity eviction, enforces WebSocket signaling, disallows transport changes on resume, supplies participant attachment options and disables WARP for MoQ joins. |
| [pkg/service/signal.go](../pkg/service/signal.go) | Modified | Closes the inter-node RPC stream with `ErrMoQUnavailable` when admission fails before the first response. This preserves the reason for the joining node. |
| [pkg/service/server.go](../pkg/service/server.go) | Modified | Validates MoQ on server creation, builds the shared QUIC server, and registers the native handler with the room lookup only when enabled. Startup logging includes the advertised endpoint on an enabled node. |
| [pkg/service/webtransport.go](../pkg/service/webtransport.go) | Modified | Adds native protocol registration and TLS ALPN dispatch to the existing QUIC listener. When native handlers are registered, it waits for handshake completion, routes matching connections to their handler and passes the remaining connections to HTTP/3. |
| [pkg/service/moq_test.go](../pkg/service/moq_test.go) | New | Exercises disabled-node admission for fresh joins and reconnects, plus rejection of incompatible WebRTC signaling. Dials a real listener with MoQ enabled and disabled and verifies HTTP/3 still serves requests in both cases. |
| [pkg/service/signal_test.go](../pkg/service/signal_test.go) | Modified | Adds a two-node signaling regression using a local RPC bus. Verifies that rejection by the room node closes the response channel with the original failed-precondition code and message. |

## Room membership, authorization and track lifetime

| File | Change | What changed |
| --- | --- | --- |
| [pkg/rtc/mediatrack.go](../pkg/rtc/mediatrack.go) | Modified | Reports the codec and base-layer demand of a receiver tap to dynacast and releases it when the subscription ends. This allows the publisher to resume a paused rendition after a regrant. |
| [pkg/rtc/participant_moq.go](../pkg/rtc/participant_moq.go) | New | Introduces the participant attachment and its lifecycle. Issues 30-second, single-use tickets, verifies their digest and generation, fences stale sessions, and starts a cleanup timer on detach. Maintains signaled media subscriptions, bridges DataTrack packets through participant grants, and binds an authorized pending `AddTrack` to an ingest receiver. |
| [pkg/rtc/participant.go](../pkg/rtc/participant.go) | Modified | Enables DataTracks for every MoQ participant, creates the optional attachment, routes MoQ signaling to its handler and initializes a signaling-only transport manager. Accounts for MoQ publications and connectivity, leaves disconnect timing to an established attachment, and guards WebRTC-specific data and migration operations. Also clones pending track info before updating RIDs from SDP to protect a concurrently marshaled publication snapshot. |
| [pkg/rtc/participant_signal.go](../pkg/rtc/participant_signal.go) | Modified | Adds a fresh `MoQTransportInfo` to join and reconnect responses for MoQ participants. Reconnect uses the attachment ticket logic while WebRTC participants keep the existing transport reconnect handler. |
| [pkg/rtc/transportmanager.go](../pkg/rtc/transportmanager.go) | Modified | Adds a `SignalingOnly` construction path that skips publisher and subscriber peer-connection allocation on the SFU. Centralizes send-side selection for single- and dual-PC WebRTC rooms and makes stats, shutdown, negotiation, ICE fallback and track operations handle absent peer connections. |
| [pkg/rtc/errors.go](../pkg/rtc/errors.go) | Modified | Defines `ErrNoPeerConnection` so operations that require an RTP peer connection can report its absence on a signaling-only participant. |
| [pkg/rtc/room.go](../pkg/rtc/room.go) | Modified | Adds `LookupPublishedTrack` to retrieve a current publication and publisher by track SID without registering future interest. The MoQ endpoint checks the requesting participant’s grants before delivery. |
| [pkg/rtc/subscriptionmanager.go](../pkg/rtc/subscriptionmanager.go) | Modified | Tracks bound media by publication and attachment generation. Overlapping reconnects retain publisher membership and the room’s existing speaker and quality callbacks. |
| [pkg/rtc/roomtrackmanager.go](../pkg/rtc/roomtrackmanager.go) | Modified | Adds the reverse index of MoQ subscribers by track SID and removes their recorded interest when a track disappears. Provides a lookup usable while holding the existing track-manager lock. |
| [pkg/rtc/participant_moq_test.go](../pkg/rtc/participant_moq_test.go) | New | Tests ticket consumption, expiry, generation replacement and stale detach, subscription-index cleanup, signaling-only transport behavior and preservation of the reconnect window when signaling closes. |
| [pkg/rtc/participant_internal_test.go](../pkg/rtc/participant_internal_test.go) | Modified | Adds a WebRTC regression for the pending-track clone. Applying RIDs from a publisher offer must update the pending entry while preserving the `TrackInfo` already returned for the publication response. |

## MoQ endpoint and mixed-room media

| File | Change | What changed |
| --- | --- | --- |
| [pkg/moq/endpoint.go](../pkg/moq/endpoint.go) | New | Accepts native `moqt-23` connections and creates a Trickster draft-23 server session. SETUP parses the room, SID, generation and ticket and attaches to the local room owner. Request dispatch enforces the participant namespace, grant checks, live subscription parameters and bounded in-flight work. |
| [pkg/moq/subscription.go](../pkg/moq/subscription.go) | New | Allocates server subscription aliases starting in the upper range at 2³¹. Writes subgroup headers, objects and payloads with a 15-second budget for opening and writing. Handles unsupported subscription updates on the writer goroutine so the final stream count is consistent. |
| [pkg/moq/datatrack.go](../pkg/moq/datatrack.go) | New | Binds one datagram alias to each signaled DataTrack. Reassembles bounded 1024-byte chunks per packet, forwards completed groups independently, and checks grants and attachment ownership. |
| [pkg/moq/path_quality.go](../pkg/moq/path_quality.go) | New | Samples quic-go RTT and packet-loss counters per connection and feeds the SFU quality model. Handles repeated reads and losses later reversed by an acknowledgement. |
| [pkg/moq/path_quality_test.go](../pkg/moq/path_quality_test.go) | New | Checks loss windows, repeated reads, late acknowledgements and increased RTT. |
| [pkg/moq/media_track.go](../pkg/moq/media_track.go) | New | Binds `up/<SID>/<codec>` to an authorized publication and validates incoming group starts and frame sizes. Builds each subscriber’s MSF catalog from signaled interests and grants, enforces subscription limits and connects a receiver tap to outgoing LOC objects. |
| [pkg/moq/media_track_test.go](../pkg/moq/media_track_test.go) | New | Verifies that unknown catalog metadata stays absent and that codec-specific spatial layer zero supplies dimensions and bitrate even when layers arrive out of order. Also verifies that an upstream RTP restart keeps the subscription open and clears queued samples and old assembly state. |
| [pkg/moq/rtp_tap.go](../pkg/moq/rtp_tap.go) | New | Implements an SFU downtrack that observes RTP on layer zero and passes assembled samples to a four-slot queue. Converts RTP time to microseconds, incorporates RTCP sender reports and resets its RTP sequence and timing state on receiver restart and retries throttled keyframe requests after loss or queue overflow. |
| [pkg/moq/loc.go](../pkg/moq/loc.go) | New | Encodes and reads public LOC properties for timestamps, video frame markings and audio levels. Converts a supplied timescale to microseconds and validates malformed timing or frame metadata before RTP ingest. |
| [pkg/moq/media/assembler.go](../pkg/moq/media/assembler.go) | New | Reorders RTP packets into complete encoded access units, handles gaps and keyframe recovery, and strips codec-specific transport headers and LiveKit trailers. Tracks spatial spans for layered frames and reconstructs AV1 OBU elements. |
| [pkg/moq/media/ingest_receiver.go](../pkg/moq/media/ingest_receiver.go) | New | Implements the SFU `TrackReceiver` for encoded MoQ input. Packetizes frames into RTP, maintains timing and sender reports, and supplies packets to ordinary SFU downtracks. Contains codec-specific VP8/VP9 descriptors, AV1 handling, temporal references and Opus timestamp progression. |
| [pkg/moq/media/catalog.go](../pkg/moq/media/catalog.go) | New | Defines the draft-02 MSF JSON catalog and rendition fields, including LiveKit publication identifiers. Converts between SFU MIME types, track-name codec suffixes and WebCodecs codec strings such as the H.264/H.265 profile strings. |
| [pkg/moq/media/media_test.go](../pkg/moq/media/media_test.go) | New | Tests H.264 reordering and loss, audio packet gaps and Opus timing, VP8 descriptor removal, catalog JSON round trips and codec parameter conversion. |
| [pkg/moq/media/codecs_test.go](../pkg/moq/media/codecs_test.go) | New | Exercises RTP packetizer/assembler behavior for H.265, VP9 and AV1, spatial and temporal layer handling, trailers, opaque encrypted payloads and Opus DTX timing. These utility tests extend beyond the single-rendition, unencrypted native room profile. |

## Shared SFU media code used by the bridge

| File | Change | What changed |
| --- | --- | --- |
| [pkg/sfu/connectionquality/connectionstats.go](../pkg/sfu/connectionquality/connectionstats.go) | Modified | Exposes a path score using the existing delay model, video loss weighting, quality thresholds and MOS conversion. |
| [pkg/sfu/connectionquality/scorer.go](../pkg/sfu/connectionquality/scorer.go) | Modified | Shares the delay and loss calculation between RTP media and transport path scoring. |
| [pkg/sfu/buffer/buffer_base.go](../pkg/sfu/buffer/buffer_base.go) | Modified | Makes `GetTemporalLayerFpsForSpatial` reject negative and out-of-range indexes and return a copy of the calculator’s frame rates. Catalog construction can read the snapshot after the buffer lock is released. |
| [pkg/sfu/buffer/fps_test.go](../pkg/sfu/buffer/fps_test.go) | Modified | Proves the returned rates stay stable after the VP8 calculator changes and checks that a negative layer returns nil. |
| [pkg/sfu/streamtracker/streamtracker_dd.go](../pkg/sfu/streamtracker/streamtracker_dd.go) | Modified | Returns a cloned cumulative bitrate slice from the dependency-descriptor tracker. Callers can inspect it after the tracker releases its lock. |
| [pkg/sfu/streamtracker/streamtracker_dd_test.go](../pkg/sfu/streamtracker/streamtracker_dd_test.go) | Modified | Mutates the tracker after obtaining a bitrate snapshot and verifies the previously returned values stay intact. |
| [pkg/sfu/packettrailer/packet_trailer.go](../pkg/sfu/packettrailer/packet_trailer.go) | Modified | Factors trailer-length detection out of stripping and adds parsing and encoding of LKTS metadata. Supports user timestamps, frame IDs and user data, including the 255-byte trailer budget and aggregated NAL-unit length handling. |
| [pkg/sfu/packettrailer/av1.go](../pkg/sfu/packettrailer/av1.go) | New | Adds helpers to insert and extract a LiveKit metadata OBU in an AV1 temporal unit, including OBU and LEB128 length parsing. Preserves the order of surrounding codec data. |
| [pkg/sfu/packettrailer/packet_trailer_test.go](../pkg/sfu/packettrailer/packet_trailer_test.go) | Modified | Covers metadata encode/decode round trips, the user-data size boundary and preservation of timestamps and frame IDs. Checks AV1 metadata insertion order, extraction and unchanged payloads when metadata is absent. |

## Interoperability and network-handoff fixtures

| File | Change | What changed |
| --- | --- | --- |
| [test/moq/README.md](../test/moq/README.md) | New | Documents how to run the Go/Rust wire exchange, the SDK room matrix and the handoff simulation, including SFU setup, test CA and expected outcomes. |
| [test/moq/handoff_relay.py](../test/moq/handoff_relay.py) | New | Adds a local UDP relay for the handoff test. Maps each client path to a separate upstream socket, drops traffic in both directions during the first move and reports an unexpected QUIC v1 Initial after migration begins. |
| [test/moq/interop/interop_test.go](../test/moq/interop/interop_test.go) | New | Starts a local Go QUIC/Trickster endpoint with a temporary certificate and launches the Rust fixture. Accepts its publication and echoes the received object on a subscription over the same connection. |
| [test/moq/interop/src/main.rs](../test/moq/interop/src/main.rs) | New | Implements the Rust side of the echo exchange using moqtap draft 23 and the supplied certificate. Publishes an object, subscribes to the echo and verifies its alias, object ID and payload. |
| [test/moq/interop/go.mod](../test/moq/interop/go.mod) | New | Defines the standalone Go module for the wire test, with Go 1.27.4, quic-go v0.64.1 and Trickster v2.2.2. |
| [test/moq/interop/go.sum](../test/moq/interop/go.sum) | New | Records checksums for the standalone fixture’s Go module graph so it can be built independently of the root server module. |
| [test/moq/interop/Cargo.toml](../test/moq/interop/Cargo.toml) | New | Defines the standalone Rust test binary and selects the pinned draft-23 moqtap client and codec plus Tokio. |
| [test/moq/interop/Cargo.lock](../test/moq/interop/Cargo.lock) | New | Locks the Rust fixture’s resolved packages for the `cargo run --locked` invocation from the Go test harness. |
| [test/moq/interop/.gitignore](../test/moq/interop/.gitignore) | New | Ignores the fixture’s local Cargo `target/` build output. |
