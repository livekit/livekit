# openvidu-livekit

This fork of [livekit](https://github.com/livekit/livekit) stores the analytics [events](https://github.com/livekit/protocol/blob/main/protobufs/livekit_analytics.proto) and stats of the server in a **MongoDB** database, in the data model that the OpenVidu Dashboard reads.

## Configuration

Extend the native livekit configuration ([`.yaml` file](https://docs.livekit.io/realtime/self-hosting/deployment/#Configuration)) with the `openvidu.analytics` section. The example sets every key but `retention.rollups_daily`; apart from `enabled`, `mongo_url` and `events.store_types`, the values are the defaults:

```yaml
openvidu:
  analytics:
    enabled: true
    mongo_url: mongodb://mongoadmin:mongopassword@localhost:27017/?replicaSet=rs0&readPreference=primaryPreferred
    database: openvidu
    interval: 10s
    retention:
      events: 7d
      stats: 7d
      tracks: 7d
      entities: 90d
      rollups_5min: 7d
      rollups_hourly: 400d
    stats_detail: participant
    anonymize_ip: true
    store_metadata: false
    events:
      store_types:
        - ROOM_CREATED
        - ROOM_ENDED
        - PARTICIPANT_JOINED
        - PARTICIPANT_ACTIVE
        - PARTICIPANT_LEFT
        - PARTICIPANT_CONNECTION_ABORTED
        - EGRESS_STARTED
        - EGRESS_ENDED
    queue:
      max_events: 20000
      max_stats: 50000
    validation_action: warn
    reconciler:
      enabled: true
      interval: 60s
      max_closures_per_run: 500
    migration:
      enabled: true
      rate: 2000
      batch_size: 500
```

| Key                        | Default                                | Meaning                                                                                                                                                                                                              |
| -------------------------- | -------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `enabled`                  | `false`                                | Write analytics.                                                                                                                                                                                                     |
| `mongo_url`                | required                               | MongoDB [connection string](https://www.mongodb.com/docs/manual/reference/connection-string/). Its password is redacted in logs.                                                                                    |
| `database`                 | `openvidu`                             | Database name.                                                                                                                                                                                                       |
| `interval`                 | `10s`                                  | Period of the write batches, from `1s` to `60s`.                                                                                                                                                                     |
| `retention.events`         | `7d`                                   | Time raw events are kept.                                                                                                                                                                                            |
| `retention.stats`          | `7d`                                   | Time statistics are kept.                                                                                                                                                                                            |
| `retention.tracks`         | `7d`                                   | Time closed tracks and subscriptions are kept.                                                                                                                                                                       |
| `retention.entities`       | `90d`                                  | Time closed rooms, participants, egress, ingress and ingress sessions are kept, at least `2d` (the [aggregator](#aggregator) computes the daily unique identities from them).                                                                |
| `retention.rollups_5min`   | `7d`                                   | Time 5-minute rollups are kept.                                                                                                                                                                                      |
| `retention.rollups_hourly` | `400d`                                 | Time hourly rollups are kept.                                                                                                                                                                                        |
| `retention.rollups_daily`  | —                                      | Time daily rollups are kept, not shorter than `retention.rollups_hourly`. Absent or `0`: forever.                                                                                                                  |
| `stats_detail`             | `participant` (CE), `track` (PRO)      | `participant`: one statistics document per participant and direction every 30 s. `track`: one per track and direction.                                                                                              |
| `anonymize_ip`             | `true`                                 | Leave client IP addresses out; `false` stores them.                                                                                                                                                                  |
| `store_metadata`           | `false`                                | Store room and participant metadata and attributes. Without it, agents still keep the attributes that name them: `lk.agent_name` and `openvidu.agent*`.                                                              |
| `events.store_types`       | every type but three (see below)       | Event types kept as raw events, as a YAML list or a comma-separated string. Every event updates the entities and rollups whatever this list says; `API_CALL` is never kept raw.                                   |
| `queue.max_events`         | `20000`                                | Events queued in memory, at least `1000`. The oldest one is dropped when the queue is full, and a warning reports the drops at most once per `interval`.                                                           |
| `queue.max_stats`          | `50000`                                | Statistics queued in memory, at least `1000`, dropped and reported the same way.                                                                                                                                    |
| `validation_action`        | `warn`                                 | `warn` logs the documents that break the collection validators; `error` rejects them (used by tests).                                                                                                               |
| `reconciler.enabled`       | `true`                                 | Close the entities whose end event never arrived. With `false` they stay open until their own event, if any, arrives.                                                                                               |
| `reconciler.interval`      | `60s`                                  | Period of the reconciler runs (see [Reconciler](#reconciler)). Not shorter than `interval`.                                                                                                                         |
| `reconciler.max_closures_per_run` | `500`                           | Entities closed by one run at most; a run that reaches it continues 5 s later.                                                                                                                                       |
| `migration.enabled`        | `true`                                 | Migrate the data of the legacy analytics writer at the first start (see [Legacy data migration](#legacy-data-migration)). With `false` the migration stays pending. |
| `migration.rate`           | `2000`                                 | Legacy documents replayed per second at most; `0` is unthrottled.                                                                                                     |
| `migration.batch_size`     | `500`                                  | Legacy documents per batch, from `1` to `10000`; a batch always holds whole seconds.                                                                                   |
| `fixer_interval`           | —                                      | **Deprecated.** Sets `reconciler.interval` when that key is absent and is ignored otherwise; a warning is logged in both cases.                                                                                     |
| `expiration`               | —                                      | **Deprecated.** Sets `retention.events` and `retention.stats` when neither is set and is ignored otherwise; a warning is logged in both cases.                                                                      |

Durations are Go durations (`10s`, `168h`, `1h30m`), optionally preceded by a whole number of days (`7d`, `400d`, `1d12h`); a bare integer is read as nanoseconds. Retentions are at least `1h`, at most 2^31−1 seconds (`24855d3h14m7s`, the MongoDB TTL limit) and in whole seconds. By default every event type the server emits is kept raw except `TRACK_PUBLISHED_UPDATE`, `TRACK_MAX_SUBSCRIBED_VIDEO_QUALITY` and `TRACK_SUBSCRIBE_REQUESTED`.

Every key can also be set with an environment variable named `LIVEKIT_OPENVIDU_ANALYTICS_` plus the key path in upper case with `_` for `.`, for example `LIVEKIT_OPENVIDU_ANALYTICS_RETENTION_EVENTS=30d` or `LIVEKIT_OPENVIDU_ANALYTICS_EVENTS_STORE_TYPES=ROOM_CREATED,ROOM_ENDED`.

An invalid value stops the server at startup with an error that names the key. The effective configuration is logged at startup, with durations in the same syntax.

## What is written

At startup the server creates or updates the collections, validators and indexes of the data model, whose contract is vendored in [`openvidu/analytics/schema/contract`](./openvidu/analytics/schema/contract), and records the schema version, retention and capabilities in `schema_meta`. An `events` collection written by previous versions is renamed to `legacy_events`.

Every `interval`, each node writes one batch: rooms, participants, tracks, subscriptions, egress, ingress and ingress sessions (idempotent updates, so a replayed or reordered batch converges), raw events, statistics (`track_stats`, a time-series collection when the server supports it), 5-minute, hourly and daily rollups, and its heartbeat in `nodes`. A server with a media node label also writes its rollups into the `node:<label>` scope, except the egress and ingress counters: the events do not say which media node runs an egress or an ingress session, so those counters exist in the global scope only. A batch that fails with a transient error (network, election) is retried first in the next interval. On a graceful shutdown the queues are written for at most 5 s.

Each room records its topology from two peaks of its STANDARD sessions: the most active at once (`participants.peakActive`, N) and the most of them active at once that have sent media (`participants.peakPublishers`, P). A session has sent media from its first track published unmuted or later unmuted, until it leaves. The `topology` is `empty` (N = 0), `single_participant` (N = 1), `no_publishers` (P = 0), `one_to_one` (N = 2), `one_to_many` (P = 1) or `many_to_many`, and `features.recording` is set once an egress of the room with a `file` or `segments` output reaches `active`. When a room ends, through its `ROOM_ENDED` or the reconciler, the hourly and daily rollups of its end bucket count it in `rooms.byTopology`, `rooms.recorded` and `rooms.bySize.<range>`, the range of its N (`0-5`, `6-10`, `11-25`, `26-50`, `51-100`, `101+`). A room written by an earlier version gets no topology and is not counted.

Credentials (TURN passwords, storage keys, stream keys, tokens in URLs) are never stored: protocol messages go through an allow-list.

## Legacy data migration

Versions before 3.10 stored analytics in the `events`, `stats`, `active_entities` and `last_alive` collections. The
first start of the new version renames `events` to `legacy_events`, records `schema_meta` `_id: migration` with
`state: pending`, and migrates the legacy data in the background while live events are written as usual:

- One node at a time runs it, elected with the Redis lock `analytics-migration-lock` (TTL 60 s, refreshed after
  every batch); the other nodes retry every minute, so a node takes over when the runner dies.
- Phases: `events` (legacy events replayed through the same projections as live events, in timestamp order),
  `stats` (legacy stats through the stats pipeline), `reconcile` (the reconciler runs until nothing is left to
  close) and `cleanup` (`active_entities` and `last_alive` dropped). `legacy_events` and `stats` are never deleted:
  they expire by their own TTL, recorded as `legacyRetainedUntil`.
- Raw events keep the LiveKit `id` of the legacy event (`AE_LEG_<legacy _id>` when it had none). The close events
  the legacy fixer inserted are not replayed as events: their entity is closed by the reconciler at the fixer's
  time (`closedBy: reconciler`, `RECONCILED`); an authentic close event overrides it.
- Batches never split a second; after every batch the checkpoint (phase and last second per collection),
  `processed` and `counts` (`events`, `stats`, `synthetic`, `skipped`) are saved, so a restart resumes without
  reapplying a document. Transient MongoDB errors are retried 5 times; then the state is `failed` with the error,
  and the next start retries from the checkpoint. A replay forced again by deleting the `migration` document is a
  no-op for entities, events and rollups, but adds the legacy statistics again.
- While the migration has not completed, the reconciler leaves alone the candidates of nodes that never wrote a
  heartbeat in `nodes`: they are legacy entities the replay closes later.
- Progress is logged every 30 s (processed, total, documents per second, ETA) and on every phase.

Rollback: stop the new server, rename `legacy_events` back to `events`, drop the v2 collections and start the
previous version.

## Reconciler

Rooms, participant sessions, egress and ingress sessions stay open in the store when their end event never arrives
(a node crash, a shutdown that could not flush, a lost event). Every `reconciler.interval`, one node closes them; the
node is elected with the Redis lock `analytics-reconciler-lock` (TTL `reconciler.interval` + 5 s), or is the only node
without Redis.
A node also runs once right after startup.

- Candidates, found with indexed queries on the store only:
  - participant sessions without stats for 90 s (3 stats intervals), or, without any stats, active (joined when never
    active) for 90 s;
  - active rooms without stats for 90 s, without an open session of a kind that keeps a room open (every kind but
    `EGRESS` and `AGENT`), whose last event is older than their empty timeout plus 2 × `reconciler.interval`;
  - active rooms of a dead node without stats or events for 90 s, whatever their open sessions and empty timeout;
  - egress and ingress sessions not ended whose room closed more than 2 × `openvidu.cleanup_interval` ago on a dead
    node; LiveKit ends the others itself with real events.
- Confirmation: a node is dead when its heartbeat in `nodes` is older than 2 × `interval` and LiveKit's node registry
  does not list it as available. Participant sessions of a dead node are confirmed at once. Its rooms are confirmed
  once LiveKit's live state no longer lists their SID, since LiveKit recreates a room with the same SID on another
  node until the dead node cleanup removes it from Redis, or once their last signal is older than
  2 × `openvidu.cleanup_interval`; they close in the run that closes their last session, or in the first run after
  the cleanup. Candidates of an alive node are confirmed when they are absent from LiveKit's live state in two
  consecutive runs. A live state that cannot be read,
  or that lists no room while rooms of alive nodes receive stats (as right after a Redis restart), confirms nothing.
- Closure: the end time is the last signal of the entity, `max(lastStatsAt, lastEventAt)`; a stats flush during the
  last 30 s vetoes it. Closed documents carry `closedBy: reconciler` and the reason `RECONCILED` (egress: `failed` with
  `error: "egress lost (reconciled)"`; ingress sessions: `error`). Tracks and subscriptions of a closed participant,
  and open sessions of a closed room, close with the same end time. Counters and rollups are updated once, and one raw
  event of the close type with `synthetic: true` and `_id: AE_SYN_<entity id>` records each closure. A real close event
  arriving later corrects the end time and the reason without counting the closure again.
- Each run logs one line per closure and records itself in `schema_meta` (`_id: reconciler`).

## Aggregator

The aggregator writes the analytics values that no server can compute from its own events. Whenever analytics is
enabled, whatever `reconciler.enabled` says, one node runs it every 60 s and once right after startup; the node is
elected with the Redis lock `analytics-aggregator-lock` (TTL 65 s), or is the only node without Redis. It has no
configuration key.

- Peaks: each run counts the rooms with status `active`, the STANDARD participant sessions with status `active`, the
  egresses with status `starting`, `active` or `ending` and the ingress sessions with status `buffering` or
  `publishing`. It keeps them with `$max` as `rooms.peakActive`, `participants.peakActive`, `egress.peakActive` and
  `ingress.peakActive` in the global documents of the current bucket of `analytics_5min`, `analytics_hourly` and
  `analytics_daily`, `0` included; `ingress.peakActive` goes to the hourly and daily documents only, like every
  ingress counter. The documents of a media node scope get the room and participant counts of its label, for every
  label held by an active room or session or by a node alive within its silent window (`max(60 s, 2 × interval +
  15 s)`) that has not stopped; egress and ingress peaks stay global. Short entities that start and end between two
  runs are not counted.
- Unique identities: each run then sets the daily `uniqueIdentities` (global and per media node label) of up to 7
  closed UTC days, from 5 minutes after the day closes and inside `retention.entities`.
- A run never lowers a stored value, so two nodes during a lock handover leave the maximum. A failed run logs a warning
  and the next one waits for the regular schedule.

## Metrics

| Metric                                      | Labels                | Meaning                                         |
| ------------------------------------------- | --------------------- | ----------------------------------------------- |
| `openvidu_analytics_queue_depth`            | `queue`               | Entries waiting in the events and stats queues. |
| `openvidu_analytics_batches_total`          | `result`              | Batches by result: `success`, `retry`, `failed`. |
| `openvidu_analytics_batch_duration_seconds` |                       | Duration of a batch.                            |
| `openvidu_analytics_dropped_total`          | `queue`, `reason`     | Entries dropped: `overflow`, `error`, `unsupported`, `shutdown`. |
| `openvidu_analytics_write_errors_total`     | `collection`, `kind`  | Writes rejected by MongoDB or that the driver cannot encode. |
| `openvidu_analytics_reconciler_runs_total`  | `result`              | Reconciler runs: `ok`, `partial`, `live-state-error`, `live-state-untrusted`, `error`. |
| `openvidu_analytics_reconciler_closed_total` | `entity`             | Entities closed: `room`, `participant`, `track`, `subscription`, `egress`, `ingress_session`. |
| `openvidu_analytics_reconciler_candidates`  | `entity`              | Candidates of the last run.                     |
| `openvidu_analytics_reconciler_run_duration_seconds` |              | Duration of a reconciler run.                   |
| `openvidu_analytics_aggregator_runs_total`  | `result`              | Aggregator runs: `ok`, `error`.                 |
| `openvidu_analytics_aggregator_run_duration_seconds` |              | Duration of an aggregator run.                  |
| `openvidu_analytics_migration_state`        |                       | Legacy migration: 0 none, 1 pending, 2 running, 3 completed, 4 failed. |
| `openvidu_analytics_migration_processed_total` | `collection`       | Legacy documents replayed: `legacy_events`, `stats`. |
| `openvidu_analytics_migration_batch_seconds` |                      | Duration of a legacy migration batch.           |

## Running locally

Launch the services of [`openvidu/docker-compose-setup`](./openvidu/docker-compose-setup) (Redis, MongoDB, egress, ingress, MinIO and the optional mongo-express):

```bash
docker compose -f openvidu/docker-compose-setup/docker-compose.yml up -d
```

Run openvidu-livekit:

```bash
export LIVEKIT_CONFIG=$(cat ${PWD}/config.yaml)
go run ./cmd/server
```

Go to `http://localhost:8081` to explore the MongoDB database with mongo-express (`admin` username and `pass` password).

## Tests

- Unit tests: `go test ./openvidu/...`.
- Integration tests, which need Docker: `go test -tags integration ./openvidu/analytics/...`. They start a MongoDB replica set with Testcontainers on `openvidu/mongodb:8.0.29-r1` (`ANALYTICS_TEST_MONGO_IMAGE` overrides it), or use the server of `ANALYTICS_TEST_MONGO_URI`.
- Batch benchmark: the integration test `TestBatchPerformanceBudget` runs when `ANALYTICS_BENCH_MONGO_URI` and `ANALYTICS_BENCH_DB` name a database filled by the data generator of the OpenVidu Dashboard (`dashboard-ce/tools/data-generator`, scenario M, 7 days).
- The recorded events and stats used by the tests are described in [`openvidu/analytics/testdata`](./openvidu/analytics/testdata/README.md), and the legacy documents in [`openvidu/analytics/migration/testdata`](./openvidu/analytics/migration/testdata/README.md).
