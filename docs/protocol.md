# LiveKit RTC Protocol

LiveKit uses a WebSocket connection for signaling. Each client should start the RTC session by connecting to `/rtc`. Once connected, LiveKit uses protocol buffers to exchange messages between the server and client. The messages are detailed in [rtc.proto](../proto/rtc.proto). Client always sends `SignalRequest` and will receive `SignalResponse` from the server.

Like with other requests to LiveKit, the WebSocket connection needs to be [authenticated](authentication.md).

## Joining a room

When the WebSocket connection is established, server will first provide information about the joined room, and waits for client to perform the handshake. Flow:

1. server sends a `JoinResponse`, which includes room information, the current participant's data, and information about other participants in the room.
2. client sends `offer`, and sets its local description.
3. server accepts connection and sends an `answer`.
4. ICE connectivity is established
5. server notifies other participants of the new participant
6. server subscribes new client to existing tracks in the room

## Handling negotiations

Negotiation is use to describe the offer/answer process in WebRTC. After the initial offer/answer process to establish the connection, negotiations are needed whenever track changes are made to the current session. Because WebRTC is a peer to peer protocol, negotiations can be initiated by either party as needed.

This creates a synchronization headache, as if both sides are initiating negotiations at the exact same time, it creates a race condition in which the peers would receive unexpected responses from the other side. This is called [glare](https://tools.ietf.org/agenda/82/slides/rtcweb-10.pdf).

In LiveKit, we've added a layer of synchronization so that negotiations are more deterministic. The server is the authority determining who should be issuing offers in a negotiation cycle. When the client wants to issue an offer to the server, it needs to follow this flow.

1. client's onnegotiationneeded callback is triggered
2. client sends server a `negotiate` (`NegotiationRequest`)
3. server determines when it's ready for a new offer, and notifies client with `NegotiationResponse`
4. when client receives `negotiate` (`NegotiationResponse`), giving it permission to send an offer. Generates a new offer and sends it.
5. server accepts the offer and sends an `answer`
6. client calls `setRemoteDescription` with that session description.

now the negotiation is complete and track changes have been applied.

## Receiving subscribed tracks

When the client is subscribed to other participants tracks, it needs to coordinate the metadata (sent in `ParticipantInfo.tracks`) with the WebRTC `ontrack` callback. For a audio or video track, `TrackInfo.sid` will match `MediaStreamTrack.id`. For a data track, `TrackInfo.sid` will match `RTCDataChannel.id.toString()`.

## Publishing tracks

To publish a track, client needs to first notify server with track metadata before actually adding the track.

1. client sends `add_track` with the `cid` field set to the client generated id for the track. (`MediaStreamTrack.id` or name of data track)
2. server adds it to a list of pending tracks and sends back `track_published`
3. client receives track_published, then:
   a. for a audio/video track, client adds the `MediaStreamTrack` to its `PeerConnection`
   b .for a data track, create the channel with `PeerConnection.createDataChannel`

The above sequence will trigger `onnegotiationneeded`

## Muting/Unmuting tracks

Muting the track requires setting `MediaStreamTrack.enabled` to false, and then notifying the server with a `mute` message.

Disabling the media track simply will not remove it from the stream, but to [send packets with 0 values periodically](https://developer.mozilla.org/en-US/docs/Web/API/MediaStreamTrack/enabled). Because this will be a waste of bandwidth, LiveKit has optimizations to skip packet forwarding to other participants when the track is muted.

Follow similar steps for unmuting
