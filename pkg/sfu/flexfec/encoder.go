// Copyright 2024 LiveKit, Inc.
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

package flexfec

import (
	"sync"

	"github.com/pion/interceptor"
	pionflexfec "github.com/pion/interceptor/pkg/flexfec"
	"github.com/pion/rtp"
)

// EncoderInterceptorFactory builds EncoderInterceptors with the configured block size.
type EncoderInterceptorFactory struct {
	numMediaPackets uint32
	numFecPackets   uint32
}

// NewEncoderInterceptorFactory returns a factory for a FlexFEC-03 encoder interceptor
// that generates numFecPackets repair packets for every numMediaPackets media packets.
//
// It exists because pion's own flexfec.FecInterceptor is unsafe to use with LiveKit's
// pooled RTP payloads. That interceptor retains each media packet's payload slice *by
// reference* and only XORs the bytes once a full block has accumulated. LiveKit, however,
// returns the underlying payload buffer to a sync.Pool as soon as the packet has been
// written downstream (see pacer.Base.SendPacket -> Pool.Put). By the time pion computes
// the parity over a block, the earlier packets' buffers have already been recycled and
// overwritten, so the generated FEC protects garbage. Receivers that lose a protected
// packet then "recover" corrupt bytes and inject them into the media stream, causing
// frame corruption and PLI storms even at very low loss rates.
//
// This interceptor is behaviourally identical to pion's, except that it copies the
// payload when buffering so the parity is computed over the bytes that were actually
// sent. The copy is bounded by numMediaPackets per stream.
func NewEncoderInterceptorFactory(numMediaPackets, numFecPackets uint32) *EncoderInterceptorFactory {
	return &EncoderInterceptorFactory{
		numMediaPackets: numMediaPackets,
		numFecPackets:   numFecPackets,
	}
}

// NewInterceptor constructs a new EncoderInterceptor.
func (f *EncoderInterceptorFactory) NewInterceptor(_ string) (interceptor.Interceptor, error) {
	return &EncoderInterceptor{
		streams:         make(map[uint32]*encoderStream),
		numMediaPackets: f.numMediaPackets,
		numFecPackets:   f.numFecPackets,
	}, nil
}

type encoderStream struct {
	mu           sync.Mutex
	encoder      pionflexfec.FlexEncoder
	packetBuffer []rtp.Packet
}

// EncoderInterceptor generates a FlexFEC-03 repair stream for outgoing media.
type EncoderInterceptor struct {
	interceptor.NoOp
	mu              sync.Mutex
	streams         map[uint32]*encoderStream
	numMediaPackets uint32
	numFecPackets   uint32
}

// UnbindLocalStream removes per-stream encoder state.
func (e *EncoderInterceptor) UnbindLocalStream(info *interceptor.StreamInfo) {
	e.mu.Lock()
	delete(e.streams, info.SSRC)
	e.mu.Unlock()
}

// BindLocalStream wraps the writer so FEC repair packets are emitted alongside media.
func (e *EncoderInterceptor) BindLocalStream(
	info *interceptor.StreamInfo, writer interceptor.RTPWriter,
) interceptor.RTPWriter {
	// No FEC negotiated for this stream; pass through untouched.
	if info.PayloadTypeForwardErrorCorrection == 0 || info.SSRCForwardErrorCorrection == 0 {
		return writer
	}

	mediaSSRC := info.SSRC

	stream := &encoderStream{
		encoder: pionflexfec.FlexEncoder03Factory{}.NewEncoder(
			info.PayloadTypeForwardErrorCorrection,
			info.SSRCForwardErrorCorrection,
		),
	}

	e.mu.Lock()
	e.streams[mediaSSRC] = stream
	e.mu.Unlock()

	return interceptor.RTPWriterFunc(
		func(header *rtp.Header, payload []byte, attributes interceptor.Attributes) (int, error) {
			// Only the protected media stream feeds the encoder.
			if header.SSRC != mediaSSRC {
				return writer.Write(header, payload, attributes)
			}

			var fecPackets []rtp.Packet
			stream.mu.Lock()
			// Copy the payload: LiveKit hands us a pooled buffer that is recycled the
			// moment this write returns, so it cannot be retained by reference.
			payloadCopy := make([]byte, len(payload))
			copy(payloadCopy, payload)
			stream.packetBuffer = append(stream.packetBuffer, rtp.Packet{
				Header:  *header,
				Payload: payloadCopy,
			})
			if len(stream.packetBuffer) == int(e.numMediaPackets) {
				fecPackets = stream.encoder.EncodeFec(stream.packetBuffer, e.numFecPackets)
				stream.packetBuffer = nil
			}
			stream.mu.Unlock()

			result, err := writer.Write(header, payload, attributes)

			for i := range fecPackets {
				fecHeader := fecPackets[i].Header
				if _, fecErr := writer.Write(&fecHeader, fecPackets[i].Payload, attributes); fecErr != nil && err == nil {
					err = fecErr
				}
			}

			return result, err
		},
	)
}
