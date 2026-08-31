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
| `pkg/telemetry/pg_analytics_service.go` | new — the sink: overrides `SendStats` and `SendEvent`, delegates every other `AnalyticsService` method to upstream |
| `pkg/telemetry/pg_analytics_store.go` | new — connection pool, migration, `COPY` writer |
| `pkg/telemetry/pg_analytics_org.go` | new — the participant → organization/kind index built from analytics events |
| `pkg/config/analytics.go` | new — `analytics.postgres.*` configuration |
| `pkg/config/config.go` | `Analytics` field on `Config`, entry in `DefaultConfig` |
| `pkg/service/wire.go` | provider swapped to `telemetry.NewAnalyticsServiceFromConfig` |
| `pkg/service/server.go` | holds the analytics service, flushes telemetry stats and drains the sink on graceful shutdown |
| `pkg/service/roommanager.go` | `FlushTelemetryStats`, so shutdown can flush without server.go reaching into an unexported field |
| `pkg/service/wire_gen.go` | regenerated (`mage generate`, or `go run github.com/google/wire/cmd/wire` in `pkg/service`) |
| `config-sample.yaml` | documents the `analytics:` block |

After bumping the upstream version, re-run wire and check that
`AnalyticsService`'s four methods and `AnalyticsStat`'s byte and packet fields are
unchanged, and that `ParticipantInfo.Attributes` and `.Kind` still reach the
participant lifecycle analytics events; nothing else in the fork depends on
upstream internals.

## Configuration

With no DSN configured the server behaves exactly like upstream — no sink, no
samples, no new connections. See the `analytics:` block in `config-sample.yaml` for
every option and its default.

```yaml
analytics:
  postgres:
    dsn_file: /etc/livekit/analytics.dsn   # or dsn:, or LIVEKIT_ANALYTICS_POSTGRES_DSN
    schema: livekit_analytics
    org_attribute_key: orgId               # participant token attribute → org_id column
    org_room_name_prefix: "world-"         # cross-check org_id against the room name; "" disables
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
    id                 bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    org_id             text,                   -- billable organization; NULL when unknown
    room_name          text        NOT NULL,   -- "world-{orgId}" / "desk-{zoneId}"
    room_id            text        NOT NULL,
    participant_id     text        NOT NULL,
    participant_kind   text,                   -- 'STANDARD' | 'EGRESS' | 'INGRESS' | 'AGENT' | ...
    track_id           text        NOT NULL,
    direction          text        NOT NULL,   -- 'upstream' | 'downstream'
    primary_bytes      bigint      NOT NULL,
    retransmit_bytes   bigint      NOT NULL,
    padding_bytes      bigint      NOT NULL,
    bytes              bigint      NOT NULL GENERATED ALWAYS AS (...) STORED,
    primary_packets    bigint,                 -- packet counts behind the byte counts above
    retransmit_packets bigint,                 -- NULL only for rows written before this column existed
    padding_packets    bigint,
    packets            bigint      GENERATED ALWAYS AS (...) STORED, -- sum of the three above; NULL if any is
    sampled_at         timestamptz NOT NULL,
    inserted_at        timestamptz NOT NULL DEFAULT now(),
    node_id            text        NOT NULL,

    UNIQUE (node_id, room_id, participant_id, track_id, direction, sampled_at)
);
```

### Adding a column later

Right now every column lives directly in `CREATE TABLE`, because no server has run
this schema yet — there is no already-deployed table for a migration to upgrade in
place. Once one exists, adding a column has to go through
`ALTER TABLE ... ADD COLUMN IF NOT EXISTS <col> <type>` instead of editing
`CREATE TABLE`, and that new statement must be **nullable with no default**. A
`CREATE TABLE IF NOT EXISTS` is a no-op against a table that already exists, so
editing the column list in place would silently stop reaching any server already
running this schema; and a column with a default forces Postgres to rewrite every
existing row under a lock, which at the ~1M rows/day this table reaches turns a
routine deploy into an outage.

The one exception is a `GENERATED ALWAYS AS (...) STORED` column like `packets` or
`bytes`: Postgres has to compute and write its value for every existing row
regardless of whether it has a default, so it is not free to add once the table has
real volume either. Landing one of those is only free while the table is still
empty or near-empty; against a table with meaningful volume it needs a manual
backfill instead of `auto_migrate`.

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
- `org_id` is the organization the bytes are billed to, taken from the participant's
  own access token — see below.
- `participant_kind` is the participant's role, resolved the same way as `org_id`
  (from the participant lifecycle events the sink already receives), so that a
  rollup can exclude agents, egress and ingress from a customer's bill by name.
  `NULL` means the participant was never resolved, not "standard" — a rollup should
  treat a `NULL` the same caution as a `NULL` `org_id`, not assume it is billable.
- `primary_packets` / `retransmit_packets` / `padding_packets` are the packet
  counts behind the byte counts of the same name, from the same `AnalyticsStream`.
  `packets` sums the three the same way `bytes` sums the byte columns — use it
  rather than summing the three yourself, so a query can't forget one of them and
  silently under-convert. See
  [Converting bytes to what is actually billed](#converting-bytes-to-what-is-actually-billed).
- The `UNIQUE` constraint makes a retried `COPY` idempotent: two batches describing
  the same logical sample collide on it instead of writing the row twice. See
  [Retries and duplicates](#retries-and-duplicates).

### How `org_id` gets there

`AnalyticsStat` carries no organization: the SFU only ever sees room and participant
ids, and the `desk-{zoneId}` → organization mapping lives in tables this server does
not own. So the organization is carried in on the participant's LiveKit access token
instead.

Whoever mints that token (`apps/api`) sets an attribute named by
`analytics.postgres.org_attribute_key`, default `orgId`. The token is signed with the
LiveKit API secret, so the value is server-issued, not something a client can choose.
It arrives as `ParticipantInfo.Attributes`, and the sink indexes it from the
participant lifecycle analytics events it already receives — no database lookup, and
nothing on the media path.

`org_id` is written **per participant**, not per room. For a `downstream` row the
participant is the *subscriber*, so egress is attributed to whoever received it.

The value is a snapshot taken when the bytes moved. Renaming or re-homing an
organization later does not rewrite old rows, which is what makes an old invoice
reproducible.

Every participant is expected to carry an organization, so a row that ends up with a
NULL `org_id` is a defect. The two ways it happens are counted apart, because they
point at different things:

| Situation | Column | Counter |
| --- | --- | --- |
| token carried the attribute with a value | that value | — |
| token carried the attribute but left it empty | `NULL` | `..._samples_empty_org_total` |
| no attribute ever reached the sink | `NULL` | `..._samples_unresolved_org_total` |

A sample is never dropped for lacking an organization: the bytes are real and this
row is the only place they exist, so discarding it would understate usage rather than
merely leave it unattributed.

### Converting bytes to what is actually billed

The SFU counts RTP header + payload. A cloud provider bills what leaves the network
interface, which additionally includes the IP header, the UDP header and the SRTP
auth tag — 38 bytes on IPv4, added to **every packet**, not to every byte,
regardless of whether that packet carried primary media, a retransmit or padding.
So the conversion factor depends entirely on how large the packets were, which is
exactly what `packets` (and the three columns it sums) make possible to
reconstruct, per row:

```
actual_bytes = bytes + (packets × 38)
```

`packets` is `NULL` on any row where one of `primary_packets` / `retransmit_packets`
/ `padding_packets` is `NULL` — a row written before those columns existed. Skip
those rows rather than treating a `NULL` as zero: zero packets would silently
convert them as if the SFU sent nothing.

Measured over a soak ([`test/analytics_soak_test.go`](../test/analytics_soak_test.go)):

| Media mix | avg packet | recorded → actual |
| --- | --- | --- |
| audio only | 94 B | ×1.406 |
| video only | 1,014 B | ×1.037 |
| audio + video | 785 B | ×1.048 |

Same recorded byte count, up to 37 percentage points apart in real cost — a single
global multiplier is wrong for every room whose media mix differs from whatever
average it was derived from, and there is no average that is fair to a voice-only
room and a video-heavy one at the same time. Per-row packet counts make the
conversion exact instead of guessed, for whatever mix that room actually has.

### Retries and duplicates

An earlier version of this document stated that retries never duplicate because
`COPY` is atomic. `COPY` is atomic — a failed batch inserts nothing — but that
covers only a client-visible failure. If the write times out or the connection
drops **after Postgres commits but before the client learns of it**, the sink sees
an error, retries, and without a constraint the batch would land twice: an
over-bill, arriving exactly when the database is already unhealthy.

`UNIQUE (node_id, room_id, participant_id, track_id, direction, sampled_at)` closes
this: a retried batch collides on the constraint instead of duplicating. Postgres
has no `ADD CONSTRAINT IF NOT EXISTS`, so `migrate()` treats "this exact constraint
already exists" (SQLSTATE `42P07`) as success the same way it does for the other
idempotent DDL — but it does **not** treat `23505` (`unique_violation`) as
success for this one statement. For every other statement in this migration a
`23505` means two nodes raced on a catalog write; for this one it means the table
already contains rows that violate the constraint being added — real duplicates
left over from before this constraint existed. Swallowing that would leave the
table unprotected while claiming to be fixed, so it is left to fail the migration
and block startup instead, an operator has to look before the server is allowed to
serve billing data believed to be deduplicated when it is not.

### The room-name cross-check

`org_id` and the `world-{orgId}` room name are minted from the same application
organization id, by the same service, at two different moments. So they must agree,
and when they do not, one of them is wrong and some usage is about to be attributed
to the wrong organization.

With `org_room_name_prefix` set (default `world-`), the sink compares them on every
sample and counts disagreements as
`livekit_analytics_sink_org_room_mismatch_total`, with a warning throttled to one
line a minute. Rooms whose name does not start with the prefix — private desk rooms,
named after a zone — carry no organization to compare against and are skipped.

**It reports and does not correct.** The token's value is recorded either way.
Silently preferring one source would destroy the very evidence that they disagreed,
and this is exactly the case where a human has to look before anyone is invoiced.

This check is the reason `room_name` is still recorded even though the rollup groups
on `org_id`: it is the independent witness that makes a mis-minted token detectable
while it is happening, and reconstructable afterwards. The table is append-only, so a
column dropped now is evidence that cannot be recovered later.

## Reading it for billing

The rollup job in `apps/api` owns everything above this table:

- Fold the bytes into `network_usage_hourly_billed`, grouping on `org_id`. Rows with
  a NULL `org_id` are not billable as-is: route them to review rather than to an
  organization, and never to a default one.
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
| `livekit_analytics_sink_samples_unresolved_org_total` | samples whose token carried no organization attribute at all — **tokens, a deploy or the participant index is broken**, alert on it |
| `livekit_analytics_sink_samples_empty_org_total` | samples whose token carried the attribute but left it empty — also a defect, counted apart to say which side is at fault |
| `livekit_analytics_sink_org_room_mismatch_total` | samples whose token organization disagreed with the room name — **usage may be attributed to the wrong organization**, alert on it |
| `livekit_analytics_sink_org_index_participants` | participants held in the participant → organization index; a number that only grows is a leak |

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

On graceful shutdown, in order: the room manager stops (every room closes, so
nothing can produce a new stat), the server flushes every stats worker's buffered
samples (`RoomManager.FlushTelemetryStats`, rather than waiting for
`telemetryService`'s own 30s ticker, which nothing else calls on shutdown), and only
then is the sink drained. Getting the flush in before the drain matters: without it,
up to one telemetry flush interval (30s) of every participant's usage still sitting
in a stats worker at the moment of shutdown was silently discarded on every graceful
restart. `TestAnalyticsSinkShutdownFlush` in
[`test/analytics_sink_test.go`](../test/analytics_sink_test.go) is the regression
test for this.

## Tests

`go test ./pkg/telemetry/ ./pkg/config/` covers the mapping, buffering, eviction,
backoff and configuration rules without a database.

The migration and `COPY` path is exercised against a real Postgres when a DSN is
provided; it creates and drops its own `livekit_analytics_test` schema:

```sh
LIVEKIT_TEST_ANALYTICS_POSTGRES_DSN=postgres://postgres:postgres@localhost:5432/postgres \
  go test ./pkg/telemetry/ -run TestStoreWritesSamples -v
```
