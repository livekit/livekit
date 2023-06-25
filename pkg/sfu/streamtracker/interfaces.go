package streamtracker

import (
	"fmt"
	"time"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
)

// ------------------------------------------------------------

type StreamStatusChange int32

func (s StreamStatusChange) String() string {
	switch s {
	case StreamStatusChangeNone:
		return "none"
	case StreamStatusChangeStopped:
		return "stopped"
	case StreamStatusChangeActive:
		return "active"
	default:
		return fmt.Sprintf("unknown: %d", int(s))
	}
}

const (
	StreamStatusChangeNone StreamStatusChange = iota
	StreamStatusChangeStopped
	StreamStatusChangeActive
)

// ------------------------------------------------------------

type StreamTrackerImpl interface {
	Start()
	Stop()
	Reset()

	GetCheckInterval() time.Duration

	Observe(hasMarker bool, ts uint32) StreamStatusChange
	CheckStatus() StreamStatusChange
}

type StreamTrackerWorker interface {
	Start()
	Stop()
	Reset()
	OnStatusChanged(f func(status StreamStatus))
	OnBitrateAvailable(f func())
	Status() StreamStatus
	BitrateTemporalCumulative() []int64
	SetPaused(paused bool)
	Observe(temporalLayer int32, pktSize int, payloadSize int, hasMarker bool, ts uint32, dd *buffer.ExtDependencyDescriptor)
}
