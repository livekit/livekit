# LiveKit SDK test server

A stateless, per-request programmable mock of the LiveKit server HTTP API. It
exists so the server SDKs (Go, Rust, Python, Node, Kotlin, Ruby) can exercise
client-side behavior against one shared implementation, published as a Docker
image and booted by each SDK's CI.

## Why it looks the way it does

- **Stateless.** All behavior is selected by per-request `X-Lk-Mock-*` headers,
  so the server holds no mutable state and tests run in parallel.
- **Multi-port = multi-region.** The process binds one listener per simulated
  region (`--ports`). A port's position in the list is its **region index**;
  index `0` is the primary the SDK is initially pointed at. `GET
  /settings/regions` advertises all of them in order.
- **One header drives every attempt.** The SDK sends the same control header on
  the initial request *and* every failover retry. Each listener decides what to
  do from its **own** index, so a single `X-Lk-Mock-Fail-Regions: 0` makes the
  primary fail while the first fallback succeeds — no coordination needed.
- **The whole API is mocked with populated responses.** Every RoomService,
  Egress, Ingress, SIP, and Connector method returns a type-correct, populated
  response: scalar fields that share a name with the request are echoed (e.g.
  `name`, `metadata`, `identity`, timeouts), `id`/`sid` fields get placeholder
  values, and list endpoints return one element. Both protobuf and JSON Twirp
  clients are supported. A client can override the response entirely with the
  `X-Lk-Mock-Response` header (see below). Unregistered/future methods fall back
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

Request headers (sent by the SDK on API calls; the SDK must forward
client-configured custom headers onto the `/settings/regions` fetch and every
failover retry):

| Header | Default | Effect |
|---|---|---|
| `X-Lk-Mock-Fail-Regions` | — | comma list of region indices that fail this request, e.g. `0` or `0,1`. Each listener fails only if its own index is listed. |
| `X-Lk-Mock-Fail-Mode` | `status` | how a failing region fails: `status`, `drop` (close connection → transport error), `delay`. |
| `X-Lk-Mock-Fail-Status` | `503` | HTTP status when failing with `status`/`delay`. |
| `X-Lk-Mock-Fail-Twirp-Code` | derived from status | Twirp error code string in the failure body. |
| `X-Lk-Mock-Delay-Ms` | `30000` | delay before a `delay`-mode region responds (for timeout tests). |
| `X-Lk-Mock-Regions-Status` | `200` | override the status of `GET /settings/regions`. |
| `X-Lk-Mock-Response` | — | protojson of the response message for the called method; replaces the populated default, giving full control over the returned payload. |

Response headers:

| Header | Meaning |
|---|---|
| `X-Lk-Mock-Region` | index of the region that served the response (blank on a failed region). Assert on this to confirm which region a failover landed on. |

## Common recipes

| Goal | Headers |
|---|---|
| Happy path | _(none)_ → 200 from region `0` |
| Failover succeeds on region 1 | `X-Lk-Mock-Fail-Regions: 0` |
| Exhaust to region 2 | `X-Lk-Mock-Fail-Regions: 0,1` |
| All regions down | `X-Lk-Mock-Fail-Regions: 0,1,2,3` |
| 4xx, no retry | `X-Lk-Mock-Fail-Regions: 0` + `X-Lk-Mock-Fail-Status: 400` |
| Transport-error failover | `X-Lk-Mock-Fail-Regions: 0` + `X-Lk-Mock-Fail-Mode: drop` |
| Region discovery unreachable | `X-Lk-Mock-Regions-Status: 500` |
| Custom response payload | `X-Lk-Mock-Response: {"sid":"RM_x","name":"my-room"}` |

Note: SDK region failover normally only engages for `*.livekit.cloud` hosts.
Since tests point at `127.0.0.1`, set the SDK's failover-enable option to its
forced-on value so failover engages against localhost.
