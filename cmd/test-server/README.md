# LiveKit SDK test server

A stateless, per-request programmable mock of the LiveKit server HTTP API. It
exists so the server SDKs (Go, Rust, Python, Node, Kotlin, Ruby) can exercise
client-side behavior against one shared implementation, published as a Docker
image and booted by each SDK's CI.

## Why it looks the way it does

- **Stateless.** All behavior is selected by a single per-request `X-Lk-Mock`
  header (a JSON object), so the server holds no mutable state and tests run in
  parallel.
- **Multi-port = multi-region.** The process binds one listener per simulated
  region (`--ports`). A port's position in the list is its **region index**;
  index `0` is the primary the SDK is initially pointed at. `GET
  /settings/regions` advertises all of them in order.
- **One header drives every attempt.** The SDK sends the same control header on
  the initial request *and* every failover retry. Each listener decides what to
  do from its **own** index, so a single `X-Lk-Mock: {"failRegions":[0]}` makes
  the primary fail while the first fallback succeeds — no coordination needed.
- **Realistic latency.** Methods that block in the real server block here too:
  `CreateSIPParticipant` with `wait_until_answered` and `TransferSIPParticipant`
  take ~11s before responding, so SDKs can exercise their timeouts.
- **The whole API is mocked with populated responses.** Every RoomService,
  Egress, Ingress, SIP, and Connector method returns a type-correct, populated
  response: scalar fields that share a name with the request are echoed (e.g.
  `name`, `metadata`, `identity`, timeouts), `id`/`sid` fields get placeholder
  values, and list endpoints return one element. Both protobuf and JSON Twirp
  clients are supported. A client can override the response entirely with the
  `response` field (see below). Unregistered/future methods fall back
  to an empty (all-default) message, which still decodes cleanly.

## Running

```bash
go run ./cmd/test-server                       # primary :9999, regions :10000-10002
go run ./cmd/test-server --ports 9999,10000    # primary + one fallback

# Docker
docker build -f cmd/test-server/Dockerfile -t livekit/test-server .
docker run -p 9999-10002:9999-10002 livekit/test-server
```

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `--ports` | `LK_TEST_SERVER_PORTS` | `9999,10000,10001,10002` | listener ports; index = position |
| `--advertise-host` | `LK_TEST_SERVER_ADVERTISE_HOST` | `http://127.0.0.1` | base URL used in `/settings/regions` |
| `--bind` | `LK_TEST_SERVER_BIND` | `0.0.0.0` | bind address |
| `--twirp-prefix` | `LK_TEST_SERVER_TWIRP_PREFIX` | `/twirp` | Twirp path prefix |

## Control protocol

All behavior is driven by a single `X-Lk-Mock` request header whose value is a
JSON object. The SDK sends the same header on API calls, on the
`/settings/regions` fetch, and on every failover retry (it must forward
client-configured custom headers onto all of them). Omit the header — or any
field — for normal behavior. Every field is optional:

| Field | Default | Effect |
|---|---|---|
| `failRegions` | — | array of region indices that fail this request, e.g. `[0]` or `[0,1]`. Each listener fails only if its own index is listed. |
| `failMode` | `status` | how a failing region fails: `status` (write a Twirp error), or `drop` (close the connection → transport error). |
| `failStatus` | `503` | HTTP status for a `status`-mode failure. |
| `failTwirpCode` | derived from status | Twirp error code string in the failure body. |
| `delayMs` | — | delay (ms) before responding, on success or failure. Overrides a method's natural latency — use it for timeout tests, or set it to skip a SIP method's built-in ~11s wait. |
| `regionsStatus` | `200` | override the status of `GET /settings/regions`. |
| `response` | — | the response message for the called method (a JSON object, protojson-shaped); replaces the populated default, giving full control over the returned payload. |
| `skipAuth` | `false` | `true` disables permission enforcement for the request (use for tests that aren't about authz, e.g. failover tests with a placeholder token). |
| `sipStatus` | — | fail a SIP dial method (`CreateSIPParticipant`/`TransferSIPParticipant`) with a SIP status, e.g. `{"code":486,"status":"Busy Here"}` (`status` optional). The Twirp error code and `sip_status_code`/`sip_status`/`error_details` metadata are derived from it exactly as the real server does. Composes with `delayMs` to simulate "ring, then fail". |

Example: `X-Lk-Mock: {"skipAuth":true,"failRegions":[0],"failStatus":400}`

> **Deprecated:** the older per-setting headers — `X-Lk-Mock-Fail-Regions`,
> `X-Lk-Mock-Fail-Mode` (incl. the `delay` mode), `X-Lk-Mock-Fail-Status`,
> `X-Lk-Mock-Fail-Twirp-Code`, `X-Lk-Mock-Delay-Ms`, `X-Lk-Mock-Regions-Status`,
> `X-Lk-Mock-Response`, `X-Lk-Mock-Skip-Auth` — are still honored for existing
> clients and will be removed later. When `X-Lk-Mock` is also present, its fields
> take precedence per-field. New clients should use `X-Lk-Mock` only.

Response headers:

| Header | Meaning |
|---|---|
| `X-Lk-Mock-Region` | index of the region that served the response (blank on a failed region). Assert on this to confirm which region a failover landed on. |

## Signal connection (WebSocket) mocking

The mock also speaks enough of the LiveKit signal protocol for SDKs to run
end-to-end signal-connection tests (connect, keepalive, reconnect, leave, and
the failure/timeout modes a client must classify). Unlike the Twirp API, signal
behavior is **not** selected by the `X-Lk-Mock` header — the WebSocket client
can't set request headers. Instead it is selected by a participant attribute
(`lk.mock`) in the access token (see below), so parallel tests need no shared
state.

Endpoints (both protocol versions are supported and behave identically):

| Path | Purpose |
|---|---|
| `/rtc`, `/rtc/v1` | WebSocket signal connection |
| `/rtc/validate`, `/rtc/v1/validate` | HTTP validate (the client fetches this when the WS fails to open) |

- The access token is read from the `access_token` query param (or a
  `Bearer` Authorization header) and verified against the API secret. A
  missing/malformed/expired/wrongly-signed token makes `validate` return
  **401** (and the WS refuse the upgrade).
- Wire format is **binary protobuf**: `SignalRequest` in, `SignalResponse` out.
- The v1 embedded publisher offer (`join_request` connection param) is
  **ignored** — no valid offer is required.
- Keepalive uses a short `pingTimeout=3s` / `pingInterval=1s` in the join so
  timeout tests run fast.

**Mode selection is via a participant attribute.** After the token is verified,
the server reads the `lk.mock` entry from the token's `attributes` claim
(`ClaimGrants.Attributes`, a `map[string]string`). The value of that attribute
is a stringified JSON control object whose `signal` field picks the behavior.
The `lk.mock` namespace is the attribute **key** (dot notation, matching
LiveKit's convention for internal attributes), so the value has no inner parent:

```
attribute key:   lk.mock
attribute value: {"signal":"no_pong"}
```

The control object also accepts an optional `leaveAction` field — a
`LeaveRequest_Action` enum value (`0`=DISCONNECT, `1`=RESUME, `2`=RECONNECT) —
that sets the `action` on the `LeaveRequest` the leave-sending modes emit
(`leave_when_connected`, `leave_first_message`, `leave_during_reconnect`). When
absent it defaults to `0` (DISCONNECT). Example:

```
attribute value: {"signal":"leave_when_connected","leaveAction":2}
```

If the `lk.mock` attribute is absent/empty, its value is unparseable, or its
`signal` is unknown, the mode defaults to `happy`. Both the WS handlers and the
validate handlers read the mode from this same attribute.

Behavior modes (any unknown/absent `signal` = `happy`):

| `signal` value | Effect |
|---|---|
| `happy` | validate → 200; WS sends `JoinResponse` (or `ReconnectResponse` if `reconnect=1`), pongs pings, closes cleanly (1000) on client `LeaveRequest` |
| `validate_500` | validate → 500; WS refuses upgrade with 500 |
| `validate_service_not_found` | validate → 404 with a body *without* the room marker (client → serviceNotFound); WS refuses with 404 |
| `room_not_found` | validate → 404 with body `requested room does not exist` (client → notAllowed); WS refuses with 404 |
| `no_first_message` | WS accepted, server sends nothing (client hits connect timeout) |
| `no_pong` | WS sends the join, then never pongs (client hits ping timeout) |
| `close_before_join` | WS upgrade succeeds, then ~50ms later a clean close (code 1011, empty reason) *before* any first message — unexpected closure during connect |
| `close_when_connected` | WS sends join, then ~200ms later closes with code 1011 |
| `drop_when_connected` | WS sends join, then ~200ms later abruptly drops the TCP connection with no close handshake — client observes an abnormal closure (code 1006) |
| `leave_when_connected` | WS sends join, then ~200ms later sends a `LeaveRequest` |
| `leave_first_message` | WS sends a `LeaveRequest` as the first (and only) message |
| `leave_during_reconnect` | on a `reconnect=1` connection, sends `LeaveRequest` first; otherwise behaves like `happy` |

`LeaveRequest`s carry `reason=SERVER_SHUTDOWN` and `action` from the control's
optional `leaveAction` (default `DISCONNECT (0)`).

## Permission enforcement

Every API method requires the same token grants the real LiveKit server checks
(see `pkg/service/auth.go`), so the mock doubles as a conformance check that an
SDK attaches the right permissions automatically. Tokens are parsed and verified
with the protocol's own `auth` helpers — the same code path the real server uses
— against the mock's configured API secret (default `secret`, matching
`livekit-server --dev`; override with `--api-secret` / `LK_TEST_SERVER_API_SECRET`).

- Missing, malformed, or wrongly-signed `Authorization` → `401 unauthenticated`.
- Validly-signed token without the required grant → `403 permission_denied`.
- `roomAdmin`-scoped methods also require the token's `room` to match the
  request's room; `ForwardParticipant`/`MoveParticipant` additionally require
  `destinationRoom` to match.

SDKs exercising permissions should sign tokens with the same API secret the mock
is configured with (`secret` by default).

| Grant | Methods |
|---|---|
| `video.roomCreate` | `CreateRoom`, `DeleteRoom`, all `Connector` calls |
| `video.roomList` | `ListRooms` |
| `video.roomRecord` | all `Egress` methods |
| `video.ingressAdmin` | all `Ingress` methods |
| `video.roomAdmin` (+ `room`) | room participant/data/metadata methods, `AgentDispatchService` methods |
| `video.roomAdmin` (+ `room` + `destinationRoom`) | `ForwardParticipant`, `MoveParticipant` |
| `sip.admin` | SIP trunk & dispatch-rule CRUD |
| `sip.call` | `CreateSIPParticipant`; `TransferSIPParticipant` (also needs `roomAdmin`) |

Send `X-Lk-Mock: {"skipAuth":true}` to bypass enforcement for tests that aren't
about permissions.

## Common recipes

| Goal | `X-Lk-Mock` value |
|---|---|
| Happy path | (no header) — valid token with the method's grant → 200 from region `0` |
| Bypass auth (failover tests) | `{"skipAuth":true}` |
| Missing-permission error | (no header) — token without the required grant → 403 |
| Failover succeeds on region 1 | `{"failRegions":[0]}` |
| Exhaust to region 2 | `{"failRegions":[0,1]}` |
| All regions down | `{"failRegions":[0,1,2,3]}` |
| 4xx, no retry | `{"failRegions":[0],"failStatus":400}` |
| Transport-error failover | `{"failRegions":[0],"failMode":"drop"}` |
| Timeout test | `{"delayMs":30000}` |
| Region discovery unreachable | `{"regionsStatus":500}` |
| Custom response payload | `{"response":{"sid":"RM_x","name":"my-room"}}` |
| SIP busy signal | `{"sipStatus":{"code":486,"status":"Busy Here"}}` |
| SIP carrier decline | `{"sipStatus":{"code":603}}` |

Note: SDK region failover normally only engages for `*.livekit.cloud` hosts.
Since tests point at `127.0.0.1`, set the SDK's failover-enable option to its
forced-on value so failover engages against localhost.
