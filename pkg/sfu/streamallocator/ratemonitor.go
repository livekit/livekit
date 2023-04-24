package streamallocator

import (
	"fmt"
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
	bitrateEstimate *timeseries.TimeSeries[int64]
	managedBytes    *timeseries.TimeSeries[uint32]
	unmanagedBytes  *timeseries.TimeSeries[uint32]

	// STREAM-ALLOCATOR-EXPERIMENTAL-TODO: remove after experimental
	history []string
}

func NewRateMonitor() *RateMonitor {
	return &RateMonitor{
		bitrateEstimate: timeseries.NewTimeSeries[int64](timeseries.TimeSeriesParams{
			UpdateOp: timeseries.TimeSeriesUpdateOpLatest,
			Window:   rateMonitorWindow,
		}),
		managedBytes: timeseries.NewTimeSeries[uint32](timeseries.TimeSeriesParams{
			UpdateOp: timeseries.TimeSeriesUpdateOpAdd,
			Window:   rateMonitorWindow,
		}),
		unmanagedBytes: timeseries.NewTimeSeries[uint32](timeseries.TimeSeriesParams{
			UpdateOp: timeseries.TimeSeriesUpdateOpAdd,
			Window:   rateMonitorWindow,
		}),
	}
}

func (r *RateMonitor) Update(estimate int64, managedBytesSent uint32, unmanagedBytesSent uint32) {
	now := time.Now()
	r.bitrateEstimate.AddSampleAt(estimate, now)
	r.managedBytes.AddSampleAt(managedBytesSent, now)
	r.unmanagedBytes.AddSampleAt(unmanagedBytesSent, now)
}

// STREAM-ALLOCATOR-TODO:
// This should be updated periodically to flush any pending.
// Reason is that the estimate could be higher than the actual rate by a significant amount.
// So, updating periodically to flush out samples that will not contribute to queueing would be good.
func (r *RateMonitor) GetQueuingGuess() float64 {
	_, _, _, qd := r.getRates(queueMonitorWindow)
	return qd
}

func (r *RateMonitor) getRates(monitorDuration time.Duration) (float64, float64, float64, float64) {
	threshold := time.Now().Add(-monitorDuration)
	bitrateEstimateSamples := r.bitrateEstimate.GetSamplesAfter(threshold)
	managedBytesSamples := r.managedBytes.GetSamplesAfter(threshold)
	unmanagedBytesSamples := r.unmanagedBytes.GetSamplesAfter(threshold)

	if len(bitrateEstimateSamples) == 0 || (len(managedBytesSamples)+len(unmanagedBytesSamples)) == 0 {
		return 0.0, 0.0, 0.0, 0.0
	}

	totalBitrateEstimate := getExpectedValue(bitrateEstimateSamples)
	totalManagedBits := getExpectedValue(managedBytesSamples) * 8
	totalUnmanagedBits := getExpectedValue(unmanagedBytesSamples) * 8
	totalBits := totalManagedBits + totalUnmanagedBits

	queuingDelay := float64(0.0)
	if totalBits > totalBitrateEstimate {
		latestBitrateEstimate := bitrateEstimateSamples[len(bitrateEstimateSamples)-1].Value
		excessBits := totalBits - totalBitrateEstimate
		queuingDelay = excessBits / float64(latestBitrateEstimate)
	}
	return totalBitrateEstimate, totalManagedBits, totalUnmanagedBits, queuingDelay
}

func (r *RateMonitor) UpdateHistory() {
	if len(r.history) >= 10 {
		r.history = r.history[1:]
	}

	e, m, um, qd := r.getRates(time.Second)
	r.history = append(
		r.history,
		fmt.Sprintf("t: %+v, e: %.2f, m: %.2f, um: %.2f, qd: %.2f", time.Now().Format(time.UnixDate), e, m, um, qd),
	)
}

func (r *RateMonitor) GetHistory() []string {
	return r.history
}

// ------------------------------------------------

func getExpectedValue[T int64 | uint32](samples []timeseries.TimeSeriesSample[T]) float64 {
	sum := 0.0
	for i := 1; 1 < len(samples); i++ {
		diff := samples[i].At.Sub(samples[i-1].At).Seconds()
		sum += diff * float64(samples[i-1].Value)
	}

	diff := time.Now().Sub(samples[len(samples)-1].At).Seconds()
	sum += diff * float64(samples[len(samples)-1].Value)
	return sum
}
