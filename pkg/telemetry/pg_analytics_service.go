// Copyright 2026 Hideout
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Fork addition: a self-hosted AnalyticsService sink.
//
// Upstream's analytics service ships AnalyticsStat to LiveKit Cloud's RPC endpoint,
// which self-hosted deployments do not have, so the per-room byte counters the SFU
// already produces are simply discarded. This sink keeps every other upstream
// behaviour and only overrides SendStats, writing the per-room, per-participant,
// per-track byte counts to Postgres. Those rows are the billing source of truth;
// client-reported network numbers are reconciled against them, never billed from.
//
// Integration surface with upstream is deliberately tiny: this file, its store, and
// one provider swap in pkg/service/wire.go.

package telemetry

import (
	"context"
	"time"

	prom "github.com/prometheus/client_golang/prometheus"
	"go.uber.org/atomic"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
)

const (
	directionUpstream   = "upstream"
	directionDownstream = "downstream"

	// maxWriteBackoff caps the retry delay while Postgres is unavailable.
	maxWriteBackoff = time.Minute

	// dropLogInterval throttles the "samples dropped" warning; drops come in bursts
	// and are counted in full by the metric either way.
	dropLogInterval = time.Minute

	// shutdownFlushTimeout bounds a single COPY during the final drain when the
	// caller did not supply a deadline.
	shutdownFlushTimeout = 5 * time.Second
)

var (
	promAnalyticsSamplesWritten = prom.NewCounter(prom.CounterOpts{
		Namespace: "livekit",
		Subsystem: "analytics_sink",
		Name:      "samples_written_total",
		Help:      "Media byte samples persisted to the analytics database.",
	})
	promAnalyticsSamplesDropped = prom.NewCounter(prom.CounterOpts{
		Namespace: "livekit",
		Subsystem: "analytics_sink",
		Name:      "samples_dropped_total",
		Help:      "Media byte samples discarded because the buffer was full. Any non-zero value means billable bytes were lost.",
	})
	promAnalyticsWriteErrors = prom.NewCounter(prom.CounterOpts{
		Namespace: "livekit",
		Subsystem: "analytics_sink",
		Name:      "write_errors_total",
		Help:      "Failed attempts to persist a batch of media byte samples.",
	})
	promAnalyticsPendingSamples = prom.NewGauge(prom.GaugeOpts{
		Namespace: "livekit",
		Subsystem: "analytics_sink",
		Name:      "pending_samples",
		Help:      "Media byte samples buffered in memory, waiting to be persisted.",
	})
)

func init() {
	prom.MustRegister(
		promAnalyticsSamplesWritten,
		promAnalyticsSamplesDropped,
		promAnalyticsWriteErrors,
		promAnalyticsPendingSamples,
	)
}

// DrainableAnalyticsService is implemented by analytics services that buffer samples
// in memory. The server drains them on shutdown so a graceful stop does not lose
// billable byte counts.
type DrainableAnalyticsService interface {
	Drain(ctx context.Context)
}

// pgAnalyticsService overrides SendStats and delegates every other method to the
// embedded upstream analytics service, so event reporting and the room project
// reporter behave exactly as they do without the fork.
type pgAnalyticsService struct {
	AnalyticsService

	conf   config.PostgresAnalyticsConfig
	store  *pgAnalyticsStore
	nodeID string
	logger logger.Logger

	samples chan roomByteSample
	closed  chan struct{}
	done    chan struct{}

	stopped     atomic.Bool
	stopOnce    atomic.Bool
	lastDropLog atomic.Int64

	// drainCtx is written by Drain before closing closed, and read by the writer
	// goroutine only after it observes that close, which orders the two accesses.
	drainCtx context.Context

	// writer goroutine state, never touched from other goroutines
	migrated    bool
	failures    int
	nextAttempt time.Time
}

// NewAnalyticsServiceFromConfig returns the Postgres-backed analytics sink when a
// DSN is configured, and upstream's analytics service otherwise. A configuration
// error (bad schema name, unreadable DSN file, unparseable DSN) fails startup; an
// unreachable database does not - the sink buffers and retries so that billing
// telemetry can never take media serving down with it.
func NewAnalyticsServiceFromConfig(conf *config.Config, currentNode routing.LocalNode) (AnalyticsService, error) {
	upstream := NewAnalyticsService(conf, currentNode)
	if !conf.Analytics.Postgres.IsConfigured() {
		return upstream, nil
	}

	pgConf, err := conf.Analytics.Postgres.Resolved()
	if err != nil {
		return nil, err
	}

	store, err := newPgAnalyticsStore(pgConf)
	if err != nil {
		return nil, err
	}

	a := &pgAnalyticsService{
		AnalyticsService: upstream,
		conf:             pgConf,
		store:            store,
		nodeID:           string(currentNode.NodeID()),
		logger:           logger.GetLogger().WithComponent("analytics_sink"),
		samples:          make(chan roomByteSample, pgConf.BufferSize),
		closed:           make(chan struct{}),
		done:             make(chan struct{}),
	}

	a.logger.Infow("recording media byte samples to postgres", store.logFields()...)
	go a.run()

	return a, nil
}

// SendStats fans a stats batch out into one sample per stream and buffers it. It is
// called from the telemetry flush loop, so it never blocks on the database: a full
// buffer drops samples and counts them rather than stalling telemetry for the node.
func (a *pgAnalyticsService) SendStats(_ context.Context, stats []*livekit.AnalyticsStat) {
	stopped := a.stopped.Load()

	for _, stat := range stats {
		for _, stream := range stat.Streams {
			sample, ok := a.sampleFromStat(stat, stream)
			if !ok {
				continue
			}

			if stopped {
				a.recordDropped(1)
				continue
			}

			select {
			case a.samples <- sample:
			default:
				a.recordDropped(1)
			}
		}
	}
}

// Drain stops the writer, flushes whatever is still buffered and closes the pool.
// It is safe to call once; later SendStats calls are counted as dropped.
func (a *pgAnalyticsService) Drain(ctx context.Context) {
	if !a.stopOnce.CompareAndSwap(false, true) {
		return
	}

	a.stopped.Store(true)
	a.drainCtx = ctx
	close(a.closed)

	select {
	case <-a.done:
		a.store.close()
	case <-ctx.Done():
		a.logger.Warnw("timed out flushing media byte samples on shutdown", ctx.Err())
	}
}

// sampleFromStat converts one stream of one stat into a row. Streams that moved no
// bytes are skipped: they carry no billing signal and would dominate the table.
func (a *pgAnalyticsService) sampleFromStat(
	stat *livekit.AnalyticsStat,
	stream *livekit.AnalyticsStream,
) (roomByteSample, bool) {
	if stat == nil || stream == nil {
		return roomByteSample{}, false
	}

	primary := stream.GetPrimaryBytes()
	retransmit := stream.GetRetransmitBytes()
	padding := stream.GetPaddingBytes()
	if primary+retransmit+padding == 0 {
		return roomByteSample{}, false
	}

	direction := directionDownstream
	if stat.Kind == livekit.StreamType_UPSTREAM {
		direction = directionUpstream
	}

	sampledAt := time.Now()
	if ts := stat.GetTimeStamp(); ts != nil {
		sampledAt = ts.AsTime()
	}

	return roomByteSample{
		RoomName:        stat.GetRoomName(),
		RoomID:          stat.GetRoomId(),
		ParticipantID:   stat.GetParticipantId(),
		TrackID:         stat.GetTrackId(),
		Direction:       direction,
		PrimaryBytes:    int64(primary),
		RetransmitBytes: int64(retransmit),
		PaddingBytes:    int64(padding),
		SampledAt:       sampledAt,
		NodeID:          a.nodeID,
	}, true
}

// run batches buffered samples and writes them, retrying with backoff while the
// database is unavailable.
func (a *pgAnalyticsService) run() {
	defer close(a.done)

	ticker := time.NewTicker(a.conf.FlushInterval)
	defer ticker.Stop()

	pending := make([]roomByteSample, 0, a.conf.BatchSize)
	for {
		select {
		case sample := <-a.samples:
			pending = a.appendPending(pending, sample)
			if len(pending) >= a.conf.BatchSize {
				pending = a.flush(pending)
			}

		case <-ticker.C:
			pending = a.flush(pending)

		case <-a.closed:
			pending = a.drainBuffered(pending)
			a.finalFlush(pending)
			return
		}
	}
}

// appendPending adds a sample to the retry buffer, evicting the oldest samples when
// the buffer is full so that memory stays bounded during a database outage.
func (a *pgAnalyticsService) appendPending(pending []roomByteSample, sample roomByteSample) []roomByteSample {
	pending = append(pending, sample)
	if overflow := len(pending) - a.conf.BufferSize; overflow > 0 {
		pending = pending[:copy(pending, pending[overflow:])]
		a.recordDropped(overflow)
	}
	promAnalyticsPendingSamples.Set(float64(len(pending)))
	return pending
}

// flush writes what it can and returns what is left, which is retried on a later
// tick. While the database is failing, retries are spaced out by the backoff.
func (a *pgAnalyticsService) flush(pending []roomByteSample) []roomByteSample {
	if len(pending) == 0 || time.Now().Before(a.nextAttempt) {
		return pending
	}

	ctx := context.Background()
	err := a.ensureMigrated(ctx, a.conf.WriteTimeout)
	if err == nil {
		pending, err = a.writeBatches(ctx, pending, a.conf.WriteTimeout)
	}
	if err != nil {
		a.onWriteFailure(err, pending)
		return pending
	}

	promAnalyticsPendingSamples.Set(float64(len(pending)))
	return pending
}

// writeBatches writes pending in batch-sized COPYs until one fails, returning
// whatever could not be written.
func (a *pgAnalyticsService) writeBatches(
	ctx context.Context,
	pending []roomByteSample,
	timeout time.Duration,
) ([]roomByteSample, error) {
	for len(pending) > 0 {
		batch := pending
		if len(batch) > a.conf.BatchSize {
			batch = batch[:a.conf.BatchSize]
		}

		writeCtx, cancel := context.WithTimeout(ctx, timeout)
		err := a.store.insert(writeCtx, batch)
		cancel()
		if err != nil {
			return pending, err
		}

		pending = pending[:copy(pending, pending[len(batch):])]
		a.onWriteSuccess(len(batch))
	}

	return pending, nil
}

// drainBuffered moves everything still in the channel into the retry buffer.
func (a *pgAnalyticsService) drainBuffered(pending []roomByteSample) []roomByteSample {
	for {
		select {
		case sample := <-a.samples:
			pending = a.appendPending(pending, sample)
		default:
			return pending
		}
	}
}

// finalFlush makes one best-effort pass over the remaining samples at shutdown. It
// ignores the retry backoff, and gives up rather than delaying exit.
func (a *pgAnalyticsService) finalFlush(pending []roomByteSample) {
	if len(pending) == 0 {
		return
	}

	ctx := a.drainCtx
	if ctx == nil {
		ctx = context.Background()
	}

	err := a.ensureMigrated(ctx, shutdownFlushTimeout)
	if err == nil {
		pending, err = a.writeBatches(ctx, pending, shutdownFlushTimeout)
	}
	if err != nil {
		promAnalyticsWriteErrors.Inc()
		a.recordDropped(len(pending))
		a.logger.Errorw("could not write media byte samples on shutdown", err, "samples", len(pending))
	}

	promAnalyticsPendingSamples.Set(float64(len(pending)))
}

// ensureMigrated runs the migration once, on the writer goroutine, so that a
// database that was unreachable at startup is picked up as soon as it comes back.
func (a *pgAnalyticsService) ensureMigrated(ctx context.Context, timeout time.Duration) error {
	if a.migrated || !a.conf.AutoMigrate {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := a.store.migrate(ctx); err != nil {
		return err
	}

	a.migrated = true
	a.logger.Infow("analytics schema ready", a.store.logFields()...)
	return nil
}

func (a *pgAnalyticsService) onWriteSuccess(written int) {
	promAnalyticsSamplesWritten.Add(float64(written))
	if a.failures > 0 {
		a.logger.Infow("analytics writes recovered", "afterFailures", a.failures)
		a.failures = 0
		a.nextAttempt = time.Time{}
	}
}

// onWriteFailure schedules the next attempt. Samples stay buffered, so a failure
// delays billing data rather than losing it - until the buffer fills up.
func (a *pgAnalyticsService) onWriteFailure(err error, pending []roomByteSample) {
	promAnalyticsWriteErrors.Inc()
	promAnalyticsPendingSamples.Set(float64(len(pending)))
	a.failures++
	a.nextAttempt = time.Now().Add(backoffFor(a.failures, a.conf.FlushInterval))
	a.logger.Errorw(
		"could not write media byte samples", err,
		"pendingSamples", len(pending),
		"consecutiveFailures", a.failures,
		"retryIn", time.Until(a.nextAttempt),
	)
}

func (a *pgAnalyticsService) recordDropped(count int) {
	if count <= 0 {
		return
	}

	promAnalyticsSamplesDropped.Add(float64(count))

	now := time.Now()
	last := a.lastDropLog.Load()
	if now.UnixNano()-last < int64(dropLogInterval) {
		return
	}
	if !a.lastDropLog.CompareAndSwap(last, now.UnixNano()) {
		return
	}
	a.logger.Warnw("dropping media byte samples, billing data is being lost", nil, "samples", count)
}

// backoffFor grows the retry delay exponentially from one flush interval up to
// maxWriteBackoff.
func backoffFor(failures int, base time.Duration) time.Duration {
	backoff := base
	for i := 1; i < failures && backoff < maxWriteBackoff; i++ {
		backoff *= 2
	}
	if backoff > maxWriteBackoff {
		return maxWriteBackoff
	}
	return backoff
}
