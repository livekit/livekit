package streamtracker

import "time"

type StreamTrackerImpl interface {
	Start()
	Stop()
	Reset()

	GetCheckInterval() time.Duration

	Observe(temporalLayer int32, pktSize int, payloadSize int) bool
	CheckStatus() StreamStatus
}
