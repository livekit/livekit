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

package rtpstats

import (
	"math/rand"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/logger"
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

func Test_RTPStatsReceiver_Update(t *testing.T) {
	clockRate := uint32(90000)
	r := NewRTPStatsReceiver(RTPStatsParams{
		ClockRate: clockRate,
		Logger:    logger.GetLogger(),
	})

	sequenceNumber := uint16(rand.Float64() * float64(1<<16))
	timestamp := uint32(rand.Float64() * float64(1<<32))
	packet := getPacket(sequenceNumber, timestamp, 1000)
	flowState := r.Update(
		time.Now().UnixNano(),
		packet.Header.SequenceNumber,
		packet.Header.Timestamp,
		packet.Header.Marker,
		packet.Header.MarshalSize(),
		len(packet.Payload),
		0,
	)
	require.True(t, r.initialized)
	require.Equal(t, sequenceNumber, r.sequenceNumber.GetHighest())
	require.Equal(t, sequenceNumber, uint16(r.sequenceNumber.GetExtendedHighest()))
	require.Equal(t, timestamp, r.timestamp.GetHighest())
	require.Equal(t, timestamp, uint32(r.timestamp.GetExtendedHighest()))

	// in-order, no loss
	sequenceNumber++
	timestamp += 3000
	packet = getPacket(sequenceNumber, timestamp, 1000)
	flowState = r.Update(
		time.Now().UnixNano(),
		packet.Header.SequenceNumber,
		packet.Header.Timestamp,
		packet.Header.Marker,
		packet.Header.MarshalSize(),
		len(packet.Payload),
		0,
	)
	require.Equal(t, sequenceNumber, r.sequenceNumber.GetHighest())
	require.Equal(t, sequenceNumber, uint16(r.sequenceNumber.GetExtendedHighest()))
	require.Equal(t, timestamp, r.timestamp.GetHighest())
	require.Equal(t, timestamp, uint32(r.timestamp.GetExtendedHighest()))

	// out-of-order, would cause a restart which is disallowed
	packet = getPacket(sequenceNumber-10, timestamp-30000, 1000)
	flowState = r.Update(
		time.Now().UnixNano(),
		packet.Header.SequenceNumber,
		packet.Header.Timestamp,
		packet.Header.Marker,
		packet.Header.MarshalSize(),
		len(packet.Payload),
		0,
	)
	require.True(t, flowState.IsNotHandled)
	require.Equal(t, sequenceNumber, r.sequenceNumber.GetHighest())
	require.Equal(t, sequenceNumber, uint16(r.sequenceNumber.GetExtendedHighest()))
	require.Equal(t, timestamp, r.timestamp.GetHighest())
	require.Equal(t, timestamp, uint32(r.timestamp.GetExtendedHighest()))
	require.Equal(t, uint64(0), r.packetsOutOfOrder)
	require.Equal(t, uint64(0), r.packetsDuplicate)

	// duplicate of the above out-of-order packet, but would not be handled as it causes a restart
	packet = getPacket(sequenceNumber-10, timestamp-30000, 1000)
	flowState = r.Update(
		time.Now().UnixNano(),
		packet.Header.SequenceNumber,
		packet.Header.Timestamp,
		packet.Header.Marker,
		packet.Header.MarshalSize(),
		len(packet.Payload),
		0,
	)
	require.True(t, flowState.IsNotHandled)
	require.Equal(t, sequenceNumber, r.sequenceNumber.GetHighest())
	require.Equal(t, sequenceNumber, uint16(r.sequenceNumber.GetExtendedHighest()))
	require.Equal(t, timestamp, r.timestamp.GetHighest())
	require.Equal(t, timestamp, uint32(r.timestamp.GetExtendedHighest()))
	require.Equal(t, uint64(0), r.packetsOutOfOrder)
	require.Equal(t, uint64(0), r.packetsDuplicate)

	// loss
	sequenceNumber += 10
	timestamp += 30000
	packet = getPacket(sequenceNumber, timestamp, 1000)
	flowState = r.Update(
		time.Now().UnixNano(),
		packet.Header.SequenceNumber,
		packet.Header.Timestamp,
		packet.Header.Marker,
		packet.Header.MarshalSize(),
		len(packet.Payload),
		0,
	)
	require.Equal(t, uint64(sequenceNumber-9), flowState.LossStartInclusive)
	require.Equal(t, uint64(sequenceNumber), flowState.LossEndExclusive)
	require.Equal(t, uint64(9), r.packetsLost)

	// out-of-order should decrement number of lost packets
	packet = getPacket(sequenceNumber-6, timestamp-18000, 1000)
	flowState = r.Update(
		time.Now().UnixNano(),
		packet.Header.SequenceNumber,
		packet.Header.Timestamp,
		packet.Header.Marker,
		packet.Header.MarshalSize(),
		len(packet.Payload),
		0,
	)
	require.Equal(t, sequenceNumber, r.sequenceNumber.GetHighest())
	require.Equal(t, sequenceNumber, uint16(r.sequenceNumber.GetExtendedHighest()))
	require.Equal(t, timestamp, r.timestamp.GetHighest())
	require.Equal(t, timestamp, uint32(r.timestamp.GetExtendedHighest()))
	require.Equal(t, uint64(1), r.packetsOutOfOrder)
	require.Equal(t, uint64(0), r.packetsDuplicate)
	require.Equal(t, uint64(8), r.packetsLost)

	// test sequence number history
	// with a gap
	sequenceNumber += 2
	timestamp += 6000
	packet = getPacket(sequenceNumber, timestamp, 1000)
	flowState = r.Update(
		time.Now().UnixNano(),
		packet.Header.SequenceNumber,
		packet.Header.Timestamp,
		packet.Header.Marker,
		packet.Header.MarshalSize(),
		len(packet.Payload),
		0,
	)
	require.Equal(t, uint64(sequenceNumber-1), flowState.LossStartInclusive)
	require.Equal(t, uint64(sequenceNumber), flowState.LossEndExclusive)
	require.Equal(t, uint64(9), r.packetsLost)
	require.False(t, r.history.IsSet(uint64(sequenceNumber)-1))

	// out-of-order
	sequenceNumber--
	timestamp -= 3000
	packet = getPacket(sequenceNumber, timestamp, 999)
	flowState = r.Update(
		time.Now().UnixNano(),
		packet.Header.SequenceNumber,
		packet.Header.Timestamp,
		packet.Header.Marker,
		packet.Header.MarshalSize(),
		len(packet.Payload),
		0,
	)
	require.Equal(t, uint64(8), r.packetsLost)
	require.Equal(t, uint64(2), r.packetsOutOfOrder)
	require.True(t, r.history.IsSet(uint64(sequenceNumber)))

	// padding only
	sequenceNumber += 2
	timestamp += 3000
	packet = getPacket(sequenceNumber, timestamp, 0)
	flowState = r.Update(
		time.Now().UnixNano(),
		packet.Header.SequenceNumber,
		packet.Header.Timestamp,
		packet.Header.Marker,
		packet.Header.MarshalSize(),
		len(packet.Payload),
		25,
	)
	require.Equal(t, uint64(8), r.packetsLost)
	require.Equal(t, uint64(2), r.packetsOutOfOrder)
	require.True(t, r.history.IsSet(uint64(sequenceNumber)))
	require.True(t, r.history.IsSet(uint64(sequenceNumber)-1))
	require.True(t, r.history.IsSet(uint64(sequenceNumber)-2))

	// old packet, but simulating increasing sequence number after roll over
	packet = getPacket(sequenceNumber+400, timestamp-6000, 300)
	flowState = r.Update(
		time.Now().UnixNano(),
		packet.Header.SequenceNumber,
		packet.Header.Timestamp,
		packet.Header.Marker,
		packet.Header.MarshalSize(),
		len(packet.Payload),
		0,
	)
	require.True(t, flowState.IsNotHandled)
	require.Equal(t, sequenceNumber, r.sequenceNumber.GetHighest())
	require.Equal(t, sequenceNumber, uint16(r.sequenceNumber.GetExtendedHighest()))
	require.Equal(t, timestamp, r.timestamp.GetHighest())
	require.Equal(t, timestamp, uint32(r.timestamp.GetExtendedHighest()))

	r.Stop()
}
