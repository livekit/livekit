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

package sfu

import (
	"sync"

	pionflexfec "github.com/pion/interceptor/pkg/flexfec"
	"github.com/pion/rtp"

	"github.com/livekit/protocol/logger"
)

// FECWriterStats accumulates downstream FlexFEC generation counters.
type FECWriterStats struct {
	PacketsSent uint64
	BytesSent   uint64
	// windows flushed early because of a sequence number discontinuity
	// (unrecovered upstream losses propagate into the forwarded stream,
	// probe padding can interleave out of order)
	PartialWindows uint64
	// single packets dropped from the window on a discontinuity, too small
	// to protect
	DiscardedSingles uint64
}

// fecWriter generates FlexFEC-03 repair packets for media sent on a
// DownTrack. Media packets are fed with their final on-wire header and
// payload (after the pacer patched extensions) so the generated FEC matches
// the bytes the subscriber receives. The flexfec encoder requires gap free
// in-order windows, on a discontinuity the consecutive run collected so far
// is protected as a partial window (with the FEC count scaled down to keep
// the overhead ratio) instead of being discarded, gaps are common when
// upstream losses could not be recovered.
type fecWriter struct {
	logger          logger.Logger
	ssrc            uint32
	payloadType     uint8
	numMediaPackets uint32
	numFECPackets   uint32

	lock     sync.Mutex
	encoder  *pionflexfec.FlexEncoder03
	window   []rtp.Packet
	nextSN   uint16
	haveNext bool
	stats    FECWriterStats
}

func newFECWriter(ssrc uint32, payloadType uint8, numMediaPackets uint32, numFECPackets uint32, logger logger.Logger) *fecWriter {
	return &fecWriter{
		logger:          logger,
		ssrc:            ssrc,
		payloadType:     payloadType,
		numMediaPackets: numMediaPackets,
		numFECPackets:   numFECPackets,
		encoder:         pionflexfec.NewFlexEncoder03(payloadType, ssrc),
		window:          make([]rtp.Packet, 0, numMediaPackets),
	}
}

// add ingests a sent media packet and returns FEC packets to transmit when a
// protection window completes. The header and payload are copied, callers
// may reuse their memory.
func (w *fecWriter) add(hdr *rtp.Header, payload []byte) []rtp.Packet {
	w.lock.Lock()
	defer w.lock.Unlock()

	var fecPackets []rtp.Packet
	if w.haveNext && hdr.SequenceNumber != w.nextSN {
		fecPackets = w.flushLocked()
		if fecPackets != nil {
			w.stats.PartialWindows++
		}
	}
	w.nextSN = hdr.SequenceNumber + 1
	w.haveNext = true

	w.window = append(w.window, rtp.Packet{
		Header:  hdr.Clone(),
		Payload: append([]byte(nil), payload...),
	})
	if len(w.window) >= int(w.numMediaPackets) {
		fecPackets = append(fecPackets, w.flushLocked()...)
	}
	return fecPackets
}

// flushLocked protects the current window and resets it. Windows of a single
// packet are dropped, a one packet XOR is just a retransmission.
func (w *fecWriter) flushLocked() []rtp.Packet {
	defer func() {
		w.window = w.window[:0]
	}()

	if len(w.window) < 2 {
		if len(w.window) == 1 {
			w.stats.DiscardedSingles++
		}
		return nil
	}

	// scale the FEC count to the window size to keep the overhead ratio
	numFEC := (w.numFECPackets*uint32(len(w.window)) + w.numMediaPackets - 1) / w.numMediaPackets
	if numFEC == 0 {
		numFEC = 1
	}

	fecPackets := w.encoder.EncodeFec(w.window, numFEC)
	for i := range fecPackets {
		w.stats.PacketsSent++
		w.stats.BytesSent += uint64(len(fecPackets[i].Payload))
	}
	return fecPackets
}

func (w *fecWriter) Stats() FECWriterStats {
	w.lock.Lock()
	defer w.lock.Unlock()

	return w.stats
}
