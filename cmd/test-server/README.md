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
