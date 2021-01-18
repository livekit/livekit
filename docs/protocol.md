# Signaling Protocol

LiveKit uses a websocket connection for signaling. Each client should start the RTC session by connecting to `/rtc`.

## Joining a room

## Handling negotiations

Negotiation is how WebRTC makes changes to the current communication, including adding/removing tracks. In WebRTC, either side could initiate the change and it makes synchronization a challenge.

In LiveKit, we've added a layer of synchronization so that negotiations are more deterministic. The server becomes the place to control who should be issuing offers in a negotiation cycle. When the client wants to issue an offer to the server _after_ the initial connection is made, this is the flow

1. client's onnegotiationneeded callback is triggered
2. client sends server a `NegotiationRequest`
3. server determines when it's ready for a new offer, and notifies client with `NegotiationResponse`
4. when client receives `NegotiationResponse`, it generates a new offer and sends it as an `offer` in the form of `SessionDescription`.
5. server accepts the offer and sends an `answer`
6. client calls `setRemoteDescription` with that session description.

now the negotiation is complete and track changes have been applied.

## Subscribing to tracks

## Publishing tracks
