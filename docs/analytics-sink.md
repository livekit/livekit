# Self-hosted analytics sink

This fork adds a Postgres sink for LiveKit's `AnalyticsService`, so the per-room byte
counters the SFU already produces are recorded instead of discarded. Those rows are
the billing source of truth: client-reported network numbers are reconciled against
them, never billed from.

Upstream's `analyticsService` streams `AnalyticsStat` to LiveKit Cloud's RPC endpoint.
Self-hosted deployments have no such endpoint, so its `stats` client is `nil` and
every stat is dropped on the floor. Node-level Prometheus counters cannot replace
them: `livekit_packet_bytes` has no room label, so once more than one room (or more
than one organization) shares a node, the bytes can no longer be split per room.

## What the fork changes

Everything new lives in files upstream does not have. Upstream files gain a handful
of lines each, which keeps `livekit-server` upgrades cheap to rebase.

| File | Change |
| --- | --- |
| `pkg/telemetry/pg_analytics_service.go` | new — the sink: overrides `SendStats`, delegates every other `AnalyticsService` method to upstream |
| `pkg/telemetry/pg_analytics_store.go` | new — connection pool, migration, `COPY` writer |
| `pkg/config/analytics.go` | new — `analytics.postgres.*` configuration |
| `pkg/config/config.go` | `Analytics` field on `Config`, entry in `DefaultConfig` |
| `pkg/service/wire.go` | provider swapped to `telemetry.NewAnalyticsServiceFromConfig` |
| `pkg/service/server.go` | holds the analytics service and drains it on graceful shutdown |
| `pkg/service/wire_gen.go` | regenerated (`mage generate`, or `go run github.com/google/wire/cmd/wire` in `pkg/service`) |
| `config-sample.yaml` | documents the `analytics:` block |

After bumping the upstream version, re-run wire and check that
`AnalyticsService`'s four methods and `AnalyticsStat`'s byte fields are unchanged;
nothing else in the fork depends on upstream internals.

## Configuration

With no DSN configured the server behaves exactly like upstream — no sink, no
samples, no new connections. See the `analytics:` block in `config-sample.yaml` for
every option and its default.

```yaml
analytics:
  postgres:
    dsn_file: /etc/livekit/analytics.dsn   # or dsn:, or LIVEKIT_ANALYTICS_POSTGRES_DSN
    schema: livekit_analytics
```

Every field is also settable as an environment variable, following the same
convention as the rest of the config: `LIVEKIT_ANALYTICS_POSTGRES_DSN`,
`LIVEKIT_ANALYTICS_POSTGRES_SCHEMA`, and so on.

The sink shares the application's database and is isolated by **schema**, not by
database. It only ever creates and writes `<schema>.room_byte_samples`. A DSN whose
role is limited to that schema is enough (plus `CREATE` on the database if
`auto_migrate` is left on; with `auto_migrate: false` the schema must be created out
of band and the sink needs only `INSERT`).

Credentials: prefer `dsn_file` (which is refused if the file is readable by others,
like key and TURN secret files) or the environment variable. The DSN is never logged;
startup logs the host, port, database, user and schema only.

## Table

```sql
CREATE TABLE livekit_analytics.room_byte_samples (
    id               bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    room_name        text        NOT NULL,   -- "world-{orgId}" / "desk-{zoneId}"
    room_id          text        NOT NULL,
    participant_id   text        NOT NULL,
    track_id         text        NOT NULL,
    direction        text        NOT NULL,   -- 'upstream' | 'downstream'
    primary_bytes    bigint      NOT NULL,
    retransmit_bytes bigint      NOT NULL,
    padding_bytes    bigint      NOT NULL,
    bytes            bigint      NOT NULL GENERATED ALWAYS AS (...) STORED,
    sampled_at       timestamptz NOT NULL,
    inserted_at      timestamptz NOT NULL DEFAULT now(),
    node_id          text        NOT NULL
);
```

Semantics a reader must know:

- **Counters are deltas** for one stats interval, not running totals. Summing rows
  over a period gives the bytes moved in that period.
- `direction` is from the SFU's point of view: `upstream` is a participant publishing
  into the SFU, `downstream` is the SFU sending to a subscriber. Egress billing is
  `downstream`.
- `bytes` is stored, not computed at read time, and splits into
  `primary_bytes` / `retransmit_bytes` / `padding_bytes` so a billing policy can
  decide whether retransmits are billable, and so disputes can be broken down.
- `sampled_at` is the flush timestamp at the *end* of the interval (upstream flushes
  every 30s), so a sample may straddle an hour boundary by up to that interval.
- Streams that moved zero bytes are not written at all.
- Rows are append-only. The sink never updates or deletes.

## Reading it for billing

The rollup job in `apps/api` owns everything above this table:

- Resolve `room_name` to an organization (`world-{orgId}` directly, `desk-{zoneId}`
  via the zone → floor → org join) and fold the bytes into
  `network_usage_hourly_billed`.
- Watermark on `inserted_at` (with a lag window covering the flush interval and
  in-flight retries), or process a closed hour after a grace period. Do not watermark
  on `sampled_at` alone: a node that was retrying can insert older samples later.
- Retries never duplicate: each batch is written with a single `COPY`, which is
  atomic, so a failed batch inserts nothing.
- Once a period is invoiced, its rows must not be re-read into a changed rollup;
  the `billed_at` watermark on `network_usage_hourly_billed` is what enforces that.
- Pruning is the rollup job's call, not the sink's — the sink never deletes samples
  it has written, so nothing unbilled can disappear underneath an invoice.

Volume: roughly one row per active track per direction every 30s, so a busy 20-person
room produces on the order of a million rows a day. Plan retention (or partitioning)
on the rollup side accordingly.

## Operations

Metrics, exported on the existing Prometheus listener:

| Metric | Meaning |
| --- | --- |
| `livekit_analytics_sink_samples_written_total` | samples persisted |
| `livekit_analytics_sink_samples_dropped_total` | samples lost — **any non-zero value means billable bytes are missing**, alert on it |
| `livekit_analytics_sink_write_errors_total` | failed batches (each is retried) |
| `livekit_analytics_sink_pending_samples` | samples buffered in memory |

Failure behaviour is fail-fast at startup, best-effort once running:

- **At startup**, `NewAnalyticsServiceFromConfig` pings the database and, with
  `auto_migrate` on, creates the schema synchronously before the server is allowed to
  finish starting. A bad schema name, unreadable DSN file, unparseable DSN,
  unreachable database, or failed migration all fail startup the same way an invalid
  key file does — an operator finds out at deploy time, not by noticing a gap in
  billing data days later.
- **Once running**, a later outage does not take the server down: media serving must
  never go down because the billing database went down. The sink buffers up to
  `buffer_size` samples in memory and retries with exponential backoff up to one
  minute. Only when the buffer is full are the oldest samples dropped, and every drop
  is counted (`livekit_analytics_sink_samples_dropped_total`).

On graceful shutdown the server drains the sink after the room manager stops, so
buffered samples are written before exit.

## Tests

`go test ./pkg/telemetry/ ./pkg/config/` covers the mapping, buffering, eviction,
backoff and configuration rules without a database.

The migration and `COPY` path is exercised against a real Postgres when a DSN is
provided; it creates and drops its own `livekit_analytics_test` schema:

```sh
LIVEKIT_TEST_ANALYTICS_POSTGRES_DSN=postgres://postgres:postgres@localhost:5432/postgres \
  go test ./pkg/telemetry/ -run TestStoreWritesSamples -v
```
