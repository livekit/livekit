package rtc

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/pion/rtp"
)

func getPacket(sn uint16, ts uint32, payloadSize int) *rtp.Packet {
	return &rtp.Packet{
		Header: rtp.Header{
			SequenceNumber: sn,
			Timestamp:      ts,
		},
		Payload: make([]byte, payloadSize),
	}
}

func TestRTPStats(t *testing.T) {
	clockRate := uint32(90000)
	windowDuration := 5 * time.Second
	r := NewRTPStats(RTPStatsParams{
		ClockRate:      clockRate,
		WindowDuration: windowDuration,
	})

	totalDuration := 4 * windowDuration
	bitrate := 1000000
	packetSize := 1000
	pps := (((bitrate + 7) / 8) + packetSize - 1) / packetSize
	framerate := 30
	sleep := 1000 / framerate
	packetsPerFrame := (pps + framerate - 1) / framerate

	sequenceNumber := uint16(rand.Float64() * float64(1<<16))
	timestamp := uint32(rand.Float64() * float64(1<<32))
	now := time.Now()
	startTime := now
	lastFrameTime := now
	for now.Sub(startTime) < totalDuration {
		timestamp += uint32(now.Sub(lastFrameTime).Seconds() * float64(clockRate))
		for i := 0; i < packetsPerFrame; i++ {
			r.Update(getPacket(sequenceNumber, timestamp, packetSize), time.Now().UnixNano())
			if (sequenceNumber % 100) == 0 {
				jump := uint16(rand.Float64() * 120.0)
				sequenceNumber += jump
			} else {
				sequenceNumber++
			}
		}

		lastFrameTime = now
		time.Sleep(time.Duration(sleep) * time.Millisecond)
		now = time.Now()
	}

	r.Stop()
	fmt.Printf("%s\n", r.ToString())
}
