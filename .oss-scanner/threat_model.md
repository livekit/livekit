# LiveKit server threat model

LiveKit server is a WebRTC SFU written in Go. Clients join rooms with a signed
access token, publish and subscribe to audio, video, and data tracks, and exchange
signalling over WebSocket or HTTP. Operators manage rooms, participants, egress,
ingress, SIP, and agent dispatch through a Twirp API authenticated by API
key/secret. The server also embeds a TURN server.

## Scope and trust boundaries

Audit the server on `master`: signalling (`pkg/service`, `pkg/rtc/signalling`),
room and participant state (`pkg/rtc`), media forwarding (`pkg/sfu`), routing
(`pkg/routing`), the agent endpoint (`pkg/agent`), WHIP, WebTransport, and TURN.

The server runs LiveKit's own Go modules in the same process, and they are in
scope on the same terms: `github.com/livekit/protocol`,
`github.com/livekit/mediatransportutil`, `github.com/livekit/psrpc`, and the forks
that `go.mod` substitutes for pion (`livekit/webrtc-pion`, `livekit/dtls`,
`livekit/ice`). Their source is in the Go module cache. Third-party dependencies,
including the rest of pion, are in scope only where untrusted input reaches them
through the server.

Untrusted parties:

- Unauthenticated network clients, against every HTTP, WebSocket, WebTransport,
  TURN, and UDP/TCP media port.
- Participants holding a valid token for one room. Their signalling messages,
  SDP, ICE candidates, RTP/RTCP, data-channel packets, metadata, and attributes
  are untrusted.
- Holders of tokens with restricted grants (subscribe-only, no data publish,
  hidden, limited `canPublishSources`, and similar).

Trusted: the operator, the server config file, the API secret, and redis.
Treat a compromised redis or a leaked API secret as out of scope.

Upstream `pion/webrtc` and `pion/dtls` are enrolled separately. A bug present in
upstream goes there; a bug only in LiveKit's forks belongs here.

## Security properties

- **Token and grant enforcement:** a token for one room must not reach another
  room. Grants must be enforced on every signalling path, including permission
  updates pushed after join. A participant must not impersonate another identity
  or act on another participant's tracks.
- **Media and data isolation:** a participant must not receive tracks or data
  packets they are not subscribed to or permitted to see. Data packets with
  destination identities must reach only those identities.
- **TURN:** relay allocations must not reach arbitrary hosts or ports beyond the
  configured relay policy.
- **Parsing:** SDP, RTP header extensions, RTCP, codec payload munging, and
  protobuf signalling must not panic, corrupt state, or read out of bounds on
  malformed input.
- **Resource use:** one participant or unauthenticated client must not exhaust
  memory, goroutines, or CPU for the whole process.

## Severity guidance

- **Critical:** unauthenticated remote code execution, or an authentication or
  room-isolation bypass that needs no valid token.
- **High:** cross-room or cross-participant access to media or data, privilege
  escalation beyond the token's grants, or a remote crash of the whole process
  that needs at most an ordinary participant token.
- **Medium:** denial of service that takes significant traffic, or affects only
  the attacker's own room. Information leaks of metadata.
- **Low / not a finding:** a participant degrading their own session; behavior
  needing the API secret, operator config, or redis access; bandwidth estimation
  or quality tuning; packet loss and reordering handled as designed.
- **Low:** a bug in a LiveKit module that is not reachable from the server's
  entry points today. State this explicitly so it can be fixed before a caller
  exposes it.

## Testing

The checkout is at `/src`. The server binary is at `/usr/local/bin/livekit-server`.
Go dependencies are cached. `GOPROXY=off` and `GOSUMDB=off` prevent downloads.
Tests expect redis on `localhost:6379`. Start it first. No docker daemon is
available, so pass `-docker=false` to `pkg/service`.

```sh
cd /src
redis-server --daemonize yes
go test -count=1 $(go list ./... | grep -v '/pkg/service$')
go test -count=1 ./pkg/service -docker=false
go test -race -count=1 ./pkg/rtc/... ./pkg/sfu/...
```

`test/` holds end-to-end tests that run real server instances against local
clients. Use them for signalling or permission reproducers. Use unit tests in
the affected package for parsing bugs.

## Reports

State the attacker's required access (none, any valid token, or a specific
grant), the entry point, and the demonstrated impact. Provide a minimal
reproducer, preferably a Go test.
For findings in dependencies, name the module and version, and say whether the
bug is reachable from the server.
