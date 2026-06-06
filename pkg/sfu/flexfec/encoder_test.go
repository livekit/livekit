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
	"testing"

	"github.com/pion/interceptor"
	pionflexfec "github.com/pion/interceptor/pkg/flexfec"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

// recyclingWriter mimics LiveKit's pooled-payload lifecycle: it captures a deep copy of
// every packet it is asked to write (the bytes that are actually sent on the wire), then
// overwrites the caller-supplied payload buffer to emulate pacer.Base.SendPacket
// returning that buffer to the sync.Pool. Any encoder that retained the payload slice by
// reference will subsequently XOR garbage.
type recyclingWriter struct {
	fecSSRC uint32
	media   []rtp.Packet
	fec     []rtp.Packet
}

func (w *recyclingWriter) Write(header *rtp.Header, payload []byte, _ interceptor.Attributes) (int, error) {
	captured := make([]byte, len(payload))
	copy(captured, payload)
	pkt := rtp.Packet{Header: *header, Payload: captured}
	if header.SSRC == w.fecSSRC {
		w.fec = append(w.fec, pkt)
	} else {
		w.media = append(w.media, pkt)
	}

	// Recycle the caller's buffer, as the pacer does once the write returns.
	for i := range payload {
		payload[i] = 0xff
	}
	return len(payload), nil
}

func fecStreamInfo() *interceptor.StreamInfo {
	return &interceptor.StreamInfo{
		SSRC:                              testMediaSSRC,
		SSRCForwardErrorCorrection:        testFECSSRC,
		PayloadTypeForwardErrorCorrection: testFECPayloadT,
	}
}

// drivePooled writes count media packets through wr. Each packet is backed by its own
// freshly allocated buffer (standing in for a pooled buffer) so that recyclingWriter can
// safely scribble over it after each write.
func drivePooled(wr interceptor.RTPWriter, baseSeq uint16, count int) {
	for i := 0; i < count; i++ {
		payloadLen := 20 + (i % 5)
		buf := make([]byte, payloadLen)
		for j := range buf {
			buf[j] = byte((i*7 + j*3) & 0xff)
		}
		hdr := &rtp.Header{
			Version:        2,
			PayloadType:    testMediaPT,
			SequenceNumber: baseSeq + uint16(i),
			Timestamp:      uint32(1000 + i*960),
			SSRC:           testMediaSSRC,
		}
		_, _ = wr.Write(hdr, buf, nil)
	}
}

// TestSFUToSubscriberFlexFECGenerationRecoversDownlinkLossWithPooledPayloads exercises the
// SFU -> subscriber leg: the SFU encoder interceptor generates a repair stream, one media
// packet is lost on the simulated downlink, and the subscriber-side decoder reconstructs
// the exact packet bytes that were actually written. It also guards the pooled-payload
// bug: the interceptor must copy payloads before retaining them for parity generation.
func TestSFUToSubscriberFlexFECGenerationRecoversDownlinkLossWithPooledPayloads(t *testing.T) {
	const numMedia, numFec = 5, uint32(1)

	wr := &recyclingWriter{fecSSRC: testFECSSRC}
	factory := NewEncoderInterceptorFactory(numMedia, numFec)
	itc, err := factory.NewInterceptor("")
	require.NoError(t, err)
	writer := itc.BindLocalStream(fecStreamInfo(), interceptor.RTPWriterFunc(wr.Write))

	drivePooled(writer, 1000, numMedia)

	require.Len(t, wr.media, numMedia, "all media packets forwarded")
	require.Len(t, wr.fec, int(numFec), "one FEC packet generated for the block")

	// Drop one media packet, deliver the rest plus FEC, and confirm the recovered
	// packet exactly matches the bytes that were sent.
	const dropped = 2
	dec := NewDecoder(testFECSSRC, testMediaSSRC)
	for i, p := range wr.media {
		if i == dropped {
			continue
		}
		require.Empty(t, dec.Decode(p))
	}

	var recovered []rtp.Packet
	for _, fp := range wr.fec {
		recovered = append(recovered, dec.Decode(fp)...)
	}

	require.Len(t, recovered, 1, "FEC must recover the single dropped packet")
	requirePacketsEqual(t, wr.media[dropped], recovered[0])
}

func TestSFUToSubscriberFlexFECGenerationUsesMultipleRepairPackets(t *testing.T) {
	const numMedia, numFec = 6, uint32(2)

	wr := &recyclingWriter{fecSSRC: testFECSSRC}
	factory := NewEncoderInterceptorFactory(numMedia, numFec)
	itc, err := factory.NewInterceptor("")
	require.NoError(t, err)
	writer := itc.BindLocalStream(fecStreamInfo(), interceptor.RTPWriterFunc(wr.Write))

	drivePooled(writer, 2000, numMedia)

	require.Len(t, wr.media, numMedia, "all media packets forwarded")
	require.Len(t, wr.fec, int(numFec), "configured number of FEC packets generated")

	// Pion's coverage is interleaved by repair-packet index, so with two FEC packets
	// one protects media indexes 0/2/4 and the other protects 1/3/5. Drop one from
	// each coverage set to prove both generated repair packets are useful.
	dropped := map[int]struct{}{
		1: {},
		4: {},
	}
	dec := NewDecoder(testFECSSRC, testMediaSSRC)
	for i, p := range wr.media {
		if _, ok := dropped[i]; ok {
			continue
		}
		require.Empty(t, dec.Decode(p))
	}

	recoveredBySeq := make(map[uint16]rtp.Packet)
	for _, fp := range wr.fec {
		for _, recovered := range dec.Decode(fp) {
			recoveredBySeq[recovered.SequenceNumber] = recovered
		}
	}

	require.Len(t, recoveredBySeq, len(dropped))
	for droppedIdx := range dropped {
		want := wr.media[droppedIdx]
		requirePacketsEqual(t, want, recoveredBySeq[want.SequenceNumber])
	}
}

// TestPionEncoderCorruptsWithPooledPayloads documents the upstream behaviour that makes
// pion's own encoder interceptor unsafe with LiveKit's pooled payloads: because it
// retains the payload by reference, recycling the buffer corrupts the parity and the
// "recovered" packet does not match what was sent. This is the bug our interceptor fixes.
func TestPionEncoderCorruptsWithPooledPayloads(t *testing.T) {
	const numMedia, numFec = 5, uint32(1)

	wr := &recyclingWriter{fecSSRC: testFECSSRC}
	factory, err := pionflexfec.NewFecInterceptor(
		pionflexfec.NumMediaPackets(numMedia),
		pionflexfec.NumFECPackets(numFec),
	)
	require.NoError(t, err)
	itc, err := factory.NewInterceptor("")
	require.NoError(t, err)
	writer := itc.BindLocalStream(fecStreamInfo(), interceptor.RTPWriterFunc(wr.Write))

	drivePooled(writer, 1000, numMedia)
	require.Len(t, wr.media, numMedia)
	require.Len(t, wr.fec, int(numFec))

	const dropped = 2
	dec := NewDecoder(testFECSSRC, testMediaSSRC)
	for i, p := range wr.media {
		if i == dropped {
			continue
		}
		dec.Decode(p)
	}
	var recovered []rtp.Packet
	for _, fp := range wr.fec {
		recovered = append(recovered, dec.Decode(fp)...)
	}

	// pion still "recovers" a packet, but its payload is garbage because the parity was
	// computed over recycled buffers.
	require.Len(t, recovered, 1)
	require.NotEqual(t, wr.media[dropped].Payload, recovered[0].Payload,
		"pion encoder is expected to produce corrupt recovery with pooled payloads")
}
