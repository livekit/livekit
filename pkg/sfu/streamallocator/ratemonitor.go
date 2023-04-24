package streamallocator

import (
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
func (r *RateMonitor) GetQueueingGuess() float64 {
	threshold := time.Now().Add(-queueMonitorWindow)
	bitrateEstimateSamples := r.bitrateEstimate.GetSamplesAfter(threshold)
	managedBytesSamples := r.managedBytes.GetSamplesAfter(threshold)
	unmanagedBytesSamples := r.unmanagedBytes.GetSamplesAfter(threshold)

	if len(bitrateEstimateSamples) == 0 || (len(managedBytesSamples)+len(unmanagedBytesSamples)) == 0 {
		return 0.0
	}

	totalBitrateEstimate := getExpectedValue(bitrateEstimateSamples)
	totalManagedBytes := getExpectedValue(managedBytesSamples)
	totalUnmanagedBytes := getExpectedValue(unmanagedBytesSamples)
	totalBits := (totalManagedBytes + totalUnmanagedBytes) * 8
	if totalBits <= totalBitrateEstimate {
		// number of bits sent in the queuing monitor window is below estimate, so no queuing
		return 0.0
	}

	latestBitrateEstimate := bitrateEstimateSamples[len(bitrateEstimateSamples)-1].Value
	excessBits := totalBits - totalBitrateEstimate
	return excessBits / float64(latestBitrateEstimate)
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
