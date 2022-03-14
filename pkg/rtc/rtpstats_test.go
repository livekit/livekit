package rtc

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
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
	r := NewRTPStats(RTPStatsParams{
		ClockRate: clockRate,
	})

	totalDuration := 5 * time.Second
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

func TestRTPStats_Update(t *testing.T) {
	clockRate := uint32(90000)
	r := NewRTPStats(RTPStatsParams{
		ClockRate: clockRate,
	})

	sequenceNumber := uint16(rand.Float64() * float64(1<<16))
	timestamp := uint32(rand.Float64() * float64(1<<32))
	isHighest, hasLoss, _, _ := r.Update(getPacket(sequenceNumber, timestamp, 1000), time.Now().UnixNano())
	require.True(t, isHighest)
	require.False(t, hasLoss)
	require.True(t, r.initialized)
	require.Equal(t, sequenceNumber, r.highestSN)
	require.Equal(t, timestamp, r.highestTS)

	// in-order, no loss
	sequenceNumber++
	timestamp += 3000
	isHighest, hasLoss, _, _ = r.Update(getPacket(sequenceNumber, timestamp, 1000), time.Now().UnixNano())
	require.True(t, isHighest)
	require.False(t, hasLoss)
	require.Equal(t, sequenceNumber, r.highestSN)
	require.Equal(t, timestamp, r.highestTS)

	// out-of-order
	isHighest, hasLoss, _, _ = r.Update(getPacket(sequenceNumber-10, timestamp-30000, 1000), time.Now().UnixNano())
	require.False(t, isHighest)
	require.False(t, hasLoss)
	require.Equal(t, sequenceNumber, r.highestSN)
	require.Equal(t, timestamp, r.highestTS)
	require.Equal(t, uint32(1), r.packetsOutOfOrder)
	require.Equal(t, uint32(0), r.packetsDuplicate)

	// duplicate
	isHighest, hasLoss, _, _ = r.Update(getPacket(sequenceNumber-10, timestamp-30000, 1000), time.Now().UnixNano())
	require.False(t, isHighest)
	require.False(t, hasLoss)
	require.Equal(t, sequenceNumber, r.highestSN)
	require.Equal(t, timestamp, r.highestTS)
	require.Equal(t, uint32(2), r.packetsOutOfOrder)
	require.Equal(t, uint32(1), r.packetsDuplicate)

	// loss
	sequenceNumber += 10
	timestamp += 30000
	isHighest, hasLoss, lossStartInclusive, lossEndExclusive := r.Update(getPacket(sequenceNumber, timestamp, 1000), time.Now().UnixNano())
	require.True(t, isHighest)
	require.True(t, hasLoss)
	require.Equal(t, lossStartInclusive, sequenceNumber-9)
	require.Equal(t, lossEndExclusive, sequenceNumber)
	require.Equal(t, uint32(17), r.packetsLost)

	// out-of-order should decrement number of lost packets
	isHighest, hasLoss, _, _ = r.Update(getPacket(sequenceNumber-15, timestamp-45000, 1000), time.Now().UnixNano())
	require.False(t, isHighest)
	require.False(t, hasLoss)
	require.Equal(t, sequenceNumber, r.highestSN)
	require.Equal(t, timestamp, r.highestTS)
	require.Equal(t, uint32(3), r.packetsOutOfOrder)
	require.Equal(t, uint32(1), r.packetsDuplicate)
	require.Equal(t, uint32(16), r.packetsLost)
	require.Equal(t, uint32(16), r.numMissingSNs(uint16(r.extStartSN), uint16(r.getExtHighestSN())))

	r.Stop()
}
