package streamtracker

import (
	"fmt"
	"time"
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
