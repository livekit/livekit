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
	_, _, _, _, _, qd := r.getRates(queueMonitorWindow)
	return qd
}

func (r *RateMonitor) getRates(monitorDuration time.Duration) (float64, float64, float64, float64, float64, float64) {
	threshold := time.Now().Add(-monitorDuration)
	bitrateEstimateSamples := r.bitrateEstimate.GetSamplesAfter(threshold)
	managedBytesSentSamples := r.managedBytesSent.GetSamplesAfter(threshold)
	managedBytesRetransmittedSamples := r.managedBytesRetransmitted.GetSamplesAfter(threshold)
	unmanagedBytesSentSamples := r.unmanagedBytesSent.GetSamplesAfter(threshold)
	unmanagedBytesRetransmittedSamples := r.unmanagedBytesRetransmitted.GetSamplesAfter(threshold)

	if len(bitrateEstimateSamples) == 0 || (len(managedBytesSentSamples)+len(managedBytesRetransmittedSamples)+len(unmanagedBytesSentSamples)+len(unmanagedBytesRetransmittedSamples)) == 0 {
		return 0.0, 0.0, 0.0, 0.0, 0.0, 0.0
	}

	totalBitrateEstimate := getTimeWeightedSum(bitrateEstimateSamples)
	totalManagedSent := getRate(managedBytesSentSamples) * 8
	totalManagedRetransmitted := getRate(managedBytesRetransmittedSamples) * 8
	totalUnmanagedSent := getRate(unmanagedBytesSentSamples) * 8
	totalUnmanagedRetransmitted := getRate(unmanagedBytesRetransmittedSamples) * 8
	totalBits := totalManagedSent + totalManagedRetransmitted + totalUnmanagedSent + totalUnmanagedRetransmitted

	queuingDelay := float64(0.0)
	if totalBits > totalBitrateEstimate {
		latestBitrateEstimate := bitrateEstimateSamples[len(bitrateEstimateSamples)-1].Value
		excessBits := totalBits - totalBitrateEstimate
		queuingDelay = excessBits / float64(latestBitrateEstimate)
	}
	return totalBitrateEstimate, totalManagedSent, totalManagedRetransmitted, totalUnmanagedSent, totalUnmanagedRetransmitted, queuingDelay
}

func (r *RateMonitor) updateHistory() {
	if len(r.history) >= 10 {
		r.history = r.history[1:]
	}

	e, m, mr, um, umr, qd := r.getRates(time.Second)
	if e == 0.0 {
		return
	}

	r.history = append(
		r.history,
		fmt.Sprintf("t: %+v, e: %.2f, m: %.2f/%.2f, um: %.2f/%.2f, qd: %.2f", time.Now().UnixMilli(), e, m, mr, um, umr, qd),
	)
}

func (r *RateMonitor) GetHistory() []string {
	return r.history
}

// ------------------------------------------------

func getTimeWeightedSum[T int64 | uint32](samples []timeseries.TimeSeriesSample[T]) float64 {
	if len(samples) < 2 {
		return 0.0
	}

	sum := 0.0
	for i := 1; i < len(samples); i++ {
		diff := samples[i].At.Sub(samples[i-1].At).Seconds()
		sum += diff * float64(samples[i-1].Value)
	}

	diff := time.Now().Sub(samples[len(samples)-1].At).Seconds()
	sum += diff * float64(samples[len(samples)-1].Value)
	return sum
}

func getRate[T int64 | uint32](samples []timeseries.TimeSeriesSample[T]) float64 {
	if len(samples) < 2 {
		return 0.0
	}

	sum := 0.0
	// start at 1 as the first sample duration is not available
	for i := 1; i < len(samples); i++ {
		sum += float64(samples[i].Value)
	}

	duration := samples[len(samples)-1].At.Sub(samples[0].At)
	if duration == 0 {
		return 0.0
	}

	return sum / duration.Seconds()
}
