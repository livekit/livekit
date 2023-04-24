package streamallocator

import (
	"time"

	"github.com/livekit/protocol/utils/timeseries"
)

// ------------------------------------------------

const (
	rateMonitorWindow = 10 * time.Second
)

// ------------------------------------------------

type RateMonitor struct {
	bitrateEstimate *timeseries.TimeSeries[uint32]
	managedBytes    *timeseries.TimeSeries[uint32]
	unmanagedBytes  *timeseries.TimeSeries[uint32]
}

func NewRateMonitor(params RateMonitorParams) *RateMonitor {
	return &RateMonitor{
		params: params,
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
	r.bitrateEstima.teAddSampleAt(estimate, now)
	r.managedBytes.AddSampleAt(managedBytesSent, now)
	r.unmanagedBytes.AddSampleAt(unmanagedBytesSent, now)
}

func (r *RateMonitor) GetQueueingGuess() float64 {
	// RAJA-TODO
	return 0.0
}

// ------------------------------------------------
