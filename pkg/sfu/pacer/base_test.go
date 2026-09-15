// Copyright 2026 LiveKit, Inc.
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

package pacer

import (
	"sync"
	"testing"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/sfu/bwe"
	"github.com/livekit/livekit-server/pkg/sfu/utils"
)

// bwe.NullBWE is meant to be embedded and lacks Type
type nullBWE struct{ *bwe.NullBWE }

func (nullBWE) Type() bwe.BWEType { return bwe.BWETypeNone }

// records extension sizes without allocating so AllocsPerRun measures only SendPacket
type extCheckWriter struct {
	writes           int
	absSendTimeLen   int
	transportWideLen int
}

func (w *extCheckWriter) WriteRTP(header *rtp.Header, _ []byte) (int, error) {
	w.writes++
	w.absSendTimeLen += len(header.GetExtension(1))
	w.transportWideLen += len(header.GetExtension(2))
	return 0, nil
}

func (w *extCheckWriter) Write(_ []byte) (int, error) { return 0, nil }

// SendPacket patches abs-send-time and transport-cc into a pooled header,
// this must not allocate per packet
func TestSendPacketHeaderExtensionsNoAlloc(t *testing.T) {
	b := NewBase(logger.GetLogger(), nullBWE{&bwe.NullBWE{}})
	headerPool := &sync.Pool{New: func() any { return &rtp.Header{} }}
	w := &extCheckWriter{}
	payload := make([]byte, 100)

	send := func() {
		hdr := headerPool.Get().(*rtp.Header)
		exts := hdr.Extensions[:0]
		*hdr = rtp.Header{Version: 2, SequenceNumber: 1, Timestamp: 2, SSRC: 3}
		hdr.Extensions = exts

		p := PacketFactory.Get().(*Packet)
		*p = Packet{
			Header:             hdr,
			HeaderPool:         headerPool,
			HeaderSize:         hdr.MarshalSize(),
			Payload:            payload,
			AbsSendTimeExtID:   1,
			TransportWideExtID: 2,
			WriteStream:        w,
		}
		_, err := b.SendPacket(p)
		require.NoError(t, err)
	}

	send() // warm the pools; AllocsPerRun also runs once before measuring
	allocs := testing.AllocsPerRun(1000, send)
	require.Equal(t, 1002, w.writes)
	require.Equal(t, 3*w.writes, w.absSendTimeLen)
	require.Equal(t, 2*w.writes, w.transportWideLen)
	if !utils.RaceEnabled {
		require.Equal(t, 0.0, allocs, "allocations per SendPacket")
	}
}
