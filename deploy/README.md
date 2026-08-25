# LiveKit Server Deployment

Deployment Guides:

- [Deploy to a VM](https://docs.livekit.io/deploy/vm)
- [Deploy to Kubernetes](https://docs.livekit.io/deploy/kubernetes)

Also included are Grafana charts for metrics gathered in Prometheus.

## Health checks

Every node answers three endpoints on its main port (`port` in the config, 7880
by default):

| Endpoint | Question | Non-200 when |
| --- | --- | --- |
| `/healthz` | is this process still working? | its stats have stopped advancing (503) |
| `/readyz` | should this node be given new work? | its keepalive is stale, or it is not `SERVING` — i.e. draining (503) |
| `/` | (legacy) is this node's keepalive current? | its keepalive is stale (`406 Not Acceptable`, not 503) |

All three are unauthenticated, and served on the signalling port rather than on
one of their own — the port the sample config recommends putting behind a load
balancer with TLS.

The difference between the first two is what matters in a multi-node
deployment. A node's keepalive is a message it publishes to itself over the
message bus, so `/readyz` and `/` both measure a round trip through Redis: when
Redis is slow or unreachable, every node in the fleet answers non-200 at the
same moment. For readiness that is the correct answer — a node that cannot
route signalling cannot host a session, and it should leave the load balancer
until it can. For liveness it is a catastrophic one: Kubernetes would restart
every container at once, and a cluster whose dependency was briefly slow would
come back with nothing able to serve. `/healthz` reads a clock the node keeps
locally and touches nothing shared, which is what makes it safe to restart on.

Point the probes accordingly:

```yaml
livenessProbe:
  httpGet:
    path: /healthz
    port: http
  periodSeconds: 10
  failureThreshold: 3
readinessProbe:
  httpGet:
    path: /readyz
    port: http
  periodSeconds: 10
  failureThreshold: 1
```

`port: http` assumes the container declares a port under that name; use the
port number if yours does not. A deployment older than these two endpoints
points both probes at `/`, and so has its liveness probe on the Redis round
trip. That is the one to move first.

A few details worth knowing when tuning the above:

- Both staleness allowances follow `node_stats.stats_update_interval` (2s by
  default) rather than assuming it, so a node told to sample less often is not
  called dead for sampling less often. Liveness allows the larger of twice that
  interval and `node_stats.stats_max_delay` (30s); readiness allows the larger
  of twice that interval and 4s, which is what `/` has always allowed.
- A draining node fails `/readyz` for as long as the drain lasts and keeps
  passing `/healthz`, so it leaves rotation without being restarted out from
  under the participants it is waiting on. `failureThreshold: 1` is what makes
  it leave on the first probe rather than the third. The cost of that setting
  is that a single late keepalive also takes every node out of rotation at
  once: existing sessions are unaffected and each node returns as soon as its
  keepalive does, but new connections have nowhere to land while it lasts.
  Raise it to 3 if you would rather ride out a blip.
- A drain waits for the participants indefinitely unless you set
  `shutdown.drain_timeout`, so by default the pod's
  `terminationGracePeriodSeconds` is what ends it, with a `SIGKILL`. Set the
  timeout below the grace period to have the server end its own drain instead,
  and set `shutdown.unreachable_drain_timeout` to end it sooner on a node that
  has stopped hearing its own keepalive — participants cannot leave a node
  whose signalling no longer routes, so that wait cannot finish.
- A node registers itself and subscribes to its own keepalive before it opens
  its port, and exits if it cannot reach Redis. Nodes already up ride an outage
  out; one that restarts during it will not come back until Redis does, and the
  kubelet will back off retrying. No probe setting changes that.
- `/` never reports the drain, only the keepalive. It answers exactly what it
  answered before the other two existed, and will keep doing so.
- A single-node deployment has no Redis and no round trip to make: the node
  stamps its own keepalive as it samples its stats. All three endpoints are
  local checks there, differing only in what they measure — `/readyz` still
  reports a drain, which is the one distinction that survives.
