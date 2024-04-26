// Copyright 2023 LiveKit, Inc.
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

package streamallocator

import (
	"fmt"
	"sync"
	"time"

	"github.com/livekit/protocol/utils/timeseries"
)

// ------------------------------------------------

const (
	rateMonitorWindow  = 10 * time.Second
	queueMonitorWindow = 2 * time.Second
)

// ------------------------------------------------

type RateMonitor struct {
	mu                          sync.Mutex
	bitrateEstimate             *timeseries.TimeSeries[int64]
	managedBytesSent            *timeseries.TimeSeries[uint32]
	managedBytesRetransmitted   *timeseries.TimeSeries[uint32]
	unmanagedBytesSent          *timeseries.TimeSeries[uint32]
	unmanagedBytesRetransmitted *timeseries.TimeSeries[uint32]

	// STREAM-ALLOCATOR-EXPERIMENTAL-TODO: remove after experimental
	history []string
}

func NewRateMonitor() *RateMonitor {
	return &RateMonitor{
		bitrateEstimate: timeseries.NewTimeSeries[int64](timeseries.TimeSeriesParams{
			UpdateOp: timeseries.TimeSeriesUpdateOpLatest,
			Window:   rateMonitorWindow,
		}),
		managedBytesSent: timeseries.NewTimeSeries[uint32](timeseries.TimeSeriesParams{
			UpdateOp: timeseries.TimeSeriesUpdateOpAdd,
			Window:   rateMonitorWindow,
		}),
		managedBytesRetransmitted: timeseries.NewTimeSeries[uint32](timeseries.TimeSeriesParams{
			UpdateOp: timeseries.TimeSeriesUpdateOpAdd,
			Window:   rateMonitorWindow,
		}),
		unmanagedBytesSent: timeseries.NewTimeSeries[uint32](timeseries.TimeSeriesParams{
			UpdateOp: timeseries.TimeSeriesUpdateOpAdd,
			Window:   rateMonitorWindow,
		}),
		unmanagedBytesRetransmitted: timeseries.NewTimeSeries[uint32](timeseries.TimeSeriesParams{
			UpdateOp: timeseries.TimeSeriesUpdateOpAdd,
			Window:   rateMonitorWindow,
		}),
	}
}

func (r *RateMonitor) Update(estimate int64, managedBytesSent uint32, managedBytesRetransmitted uint32, unmanagedBytesSent uint32, unmanagedBytesRetransmitted uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	r.bitrateEstimate.AddSampleAt(estimate, now)
	r.managedBytesSent.AddSampleAt(managedBytesSent, now)
	r.managedBytesRetransmitted.AddSampleAt(managedBytesRetransmitted, now)
	r.unmanagedBytesSent.AddSampleAt(unmanagedBytesSent, now)
	r.unmanagedBytesRetransmitted.AddSampleAt(unmanagedBytesRetransmitted, now)

	r.updateHistory()
}

// STREAM-ALLOCATOR-TODO:
// This should be updated periodically to flush any pending.
// Reason is that the estimate could be higher than the actual rate by a significant amount.
// So, updating periodically to flush out samples that will not contribute to queueing would be good.
func (r *RateMonitor) GetQueuingGuess() float64 {
	_, _, _, _, _, queuingDelay := r.getRates(queueMonitorWindow)
	return queuingDelay
}

func (r *RateMonitor) getRates(monitorDuration time.Duration) (totalBitrateEstimate, totalManagedSent, totalManagedRetransmitted, totalUnmanagedSent, totalUnmanagedRetransmitted, queuingDelay float64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	threshold := time.Now().Add(-monitorDuration)
	if !r.bitrateEstimate.HasSamplesAfter(threshold) ||
		!(r.managedBytesSent.HasSamplesAfter(threshold) ||
			r.managedBytesRetransmitted.HasSamplesAfter(threshold) ||
			r.unmanagedBytesSent.HasSamplesAfter(threshold) ||
			r.unmanagedBytesRetransmitted.HasSamplesAfter(threshold)) {
		return
	}

	totalBitrateEstimate = getTimeWeightedSum(r.bitrateEstimate.ReverseIterateSamplesAfter(threshold))
	totalManagedSent = getRate(r.managedBytesSent.ReverseIterateSamplesAfter(threshold)) * 8
	totalManagedRetransmitted = getRate(r.managedBytesRetransmitted.ReverseIterateSamplesAfter(threshold)) * 8
	totalUnmanagedSent = getRate(r.unmanagedBytesSent.ReverseIterateSamplesAfter(threshold)) * 8
	totalUnmanagedRetransmitted = getRate(r.unmanagedBytesRetransmitted.ReverseIterateSamplesAfter(threshold)) * 8
	totalBits := totalManagedSent + totalManagedRetransmitted + totalUnmanagedSent + totalUnmanagedRetransmitted

	if totalBits > totalBitrateEstimate {
		latestBitrateEstimate := r.bitrateEstimate.Back().Value
		excessBits := totalBits - totalBitrateEstimate
		queuingDelay = excessBits / float64(latestBitrateEstimate)
	}
	return
}

func (r *RateMonitor) updateHistory() {
	if len(r.history) >= 10 {
		r.history = r.history[1:]
	}

	e, m, mr, um, umr, qd := r.getRates(time.Second)
	if e == 0.0 {
		return
	}

	r.mu.Lock()
	r.history = append(
		r.history,
		fmt.Sprintf("t: %+v, e: %.2f, m: %.2f/%.2f, um: %.2f/%.2f, qd: %.2f", time.Now().UnixMilli(), e, m, mr, um, umr, qd),
	)
	r.mu.Unlock()
}

func (r *RateMonitor) GetHistory() []string {
	return r.history
}

// ------------------------------------------------

func getTimeWeightedSum[T int64 | uint32](it timeseries.ReverseIterator[T]) float64 {
	sum := 0.0
	next := time.Now()
	for it.Next() {
		diff := next.Sub(it.Value().At).Seconds()
		sum += diff * float64(it.Value().Value)
		next = it.Value().At
	}
	return sum
}

func getRate[T int64 | uint32](it timeseries.ReverseIterator[T]) float64 {
	var sum float64
	var first, last time.Time
	for it.Next() {
		if last.IsZero() {
			last = it.Value().At
		}
		first = it.Value().At
		sum += float64(it.Value().Value)
	}

	if duration := last.Sub(first); duration > 0 {
		return sum / duration.Seconds()
	}
	return 0
}
