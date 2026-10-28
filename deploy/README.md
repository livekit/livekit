# LiveKit Server Deployment

Deployment Guides:

- [Deploy to a VM](https://docs.livekit.io/deploy/vm)
- [Deploy to Kubernetes](https://docs.livekit.io/deploy/kubernetes)

Also included are Grafana charts for metrics gathered in Prometheus.

## Redis ACL rules

When LiveKit is configured with Redis (see the `redis:` block in
[config-sample.yaml](../config-sample.yaml)), it uses the connection for room
and participant state, routing, and the inter-node message bus. Managed Redis
deployments and hardened self-hosted setups often restrict what a user may do
through [ACLs](https://redis.io/docs/latest/operate/oss_and_stack/management/security/acl/).
This section lists what the server needs.

### Keys

LiveKit stores all of its state under these keys and prefixes:

| Key / prefix | Contents |
| --- | --- |
| `livekit_version` | version of the node that owns the registry |
| `rooms` | room name to room record |
| `room_internal` | room name to internal state |
| `room_node_map` | room name to the node hosting it |
| `nodes` | node ID to node registration |
| `room_participants:<room>` | participant identity to participant record |
| `room_lock:<room>` | room allocation lock |
| `agent_dispatch:<room>`, `agent_job:<room>` | agent dispatch and job records |
| `egress`, `ended_egress`, `egress:room:<room>` | egress records |
| `ingress`, `{ingress}_stream_key`, `{ingress}_state:<id>`, `room_{ingress}:<room>` | ingress records and state |

Grants for `~*` cover all of these. To restrict keys instead, grant one
pattern per key or prefix (for example `~rooms` and
`~room_participants:*`): each key is matched against each pattern in the
list, so a handful of prefix patterns covers everything without opening up
unrelated keys.

### Commands

The server uses hash commands (`HSET`, `HGET`, `HDEL`, `HGETALL`, `HMGET`,
`HVALS`, `HKEYS`), string commands (`GET`, `SET`, `SETNX`, `DEL`), set
commands (`SADD`, `SREM`, `SMEMBERS`), the lock release script (`EVAL` and
`EVALSHA`), and Redis transactions (`WATCH`, `MULTI`, `EXEC`, `DISCARD`,
`UNWATCH`). Pub/sub commands (`SUBSCRIBE`, `UNSUBSCRIBE`, `PUBLISH`) carry
the psrpc message bus.

Two go-redis behaviors shape what a first-time connection needs: the client
issues `PING` on connect and identifies itself with `CLIENT SETINFO`, so a
user without `+@connection` fails at startup with
`unable to connect to redis: NOPERM ... 'ping'`. And in cluster mode
(`cluster_addresses`), the client runs `CLUSTER SLOTS` to discover the slot
map, so the user also needs `+cluster` (or just `+cluster|slots`); without
it, startup fails with `NOPERM ... 'cluster|slots'`.

### Working rule sets

Single-node Redis (address or sentinel), verified end to end against Redis 8:

```
ACL SETUSER livekit on >mypassword ~* &* +@read +@write +@pubsub +@transaction +@scripting +@connection
```

Cluster mode, same grants plus the slot map:

```
ACL SETUSER livekit on >mypassword ~* &* +@read +@write +@pubsub +@transaction +@scripting +@connection +cluster
```

`&*` grants all pub/sub channels. The server publishes and subscribes only
on channels named `<Service>|<Method>|<topic>|<kind>` where service is one
of `Room`, `RoomManager`, `Participant`, `AgentDispatchInternal`,
`Keepalive`, `Signal`, `EgressInternal`, `IngressInternal`, or
`WHIPParticipant` (for example `Room|DeleteRoom|<room>|REQ` and
`Keepalive|Ping|<nodeID>|REQ`). To restrict channels, allow `&<service>|*`
for each of those services. The topic can be a room name or a node ID, so a
per-channel allow-list only works if room names avoid the `|` character.
