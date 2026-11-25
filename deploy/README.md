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
These are the two working rule sets, verified end to end against Redis 8.

Single-node Redis (address or sentinel):

```
ACL SETUSER livekit on >mypassword ~* &* +@read +@write +@pubsub +@transaction +@scripting +@connection
```

Cluster mode (`cluster_addresses`), the same grants plus the slot map:

```
ACL SETUSER livekit on >mypassword ~* &* +@read +@write +@pubsub +@transaction +@scripting +@connection +cluster
```

`+@connection` covers the `PING` and `CLIENT SETINFO` go-redis issues on
connect; without it startup fails with `NOPERM ... 'ping'`. `+cluster`
covers `CLUSTER SLOTS`, which the client runs in cluster mode to discover
the slot map; without it startup fails with `NOPERM ... 'cluster|slots'`.
