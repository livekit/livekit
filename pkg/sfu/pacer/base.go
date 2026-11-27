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

package pacer

import (
	"errors"
	"io"
	"time"

	"github.com/livekit/livekit-server/pkg/sfu/bwe"
	"github.com/livekit/mediatransportutil"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils/mono"
	"github.com/pion/rtp"
	"go.uber.org/atomic"
)

type Base struct {
	logger logger.Logger

	bwe bwe.BWE

	lastPacketSentAt atomic.Int64

	*ProbeObserver
}

func NewBase(logger logger.Logger, bwe bwe.BWE) *Base {
	return &Base{
		logger:        logger,
		bwe:           bwe,
		ProbeObserver: NewProbeObserver(logger),
	}
}

func (b *Base) SetInterval(_interval time.Duration) {
}

func (b *Base) SetBitrate(_bitrate int) {
}

func (b *Base) TimeSinceLastSentPacket() time.Duration {
	return time.Duration(mono.UnixNano() - b.lastPacketSentAt.Load())
}

func (b *Base) SendPacket(p *Packet) (int, error) {
	defer func() {
		if p.HeaderPool != nil && p.Header != nil {
			// keep Extensions capacity so the next user does not allocate on SetExtension
			exts := p.Header.Extensions
			clear(exts)
			*p.Header = rtp.Header{}
			p.Header.Extensions = exts[:0]
			p.HeaderPool.Put(p.Header)
		}

		if p.Pool != nil && p.PoolEntity != nil {
			p.Pool.Put(p.PoolEntity)
		}

		*p = Packet{}
		PacketFactory.Put(p)
	}()

	err := b.patchRTPHeaderExtensions(p)
	if err != nil {
		b.logger.Errorw("patching rtp header extensions err", err)
		return 0, err
	}

	var written int
	written, err = p.WriteStream.WriteRTP(p.Header, p.Payload)
	if err != nil {
		if !errors.Is(err, io.ErrClosedPipe) {
			b.logger.Errorw("write rtp packet failed", err)
		}
		return 0, err
	}

	if p.FEC != nil && written > 0 && !p.IsRTX && !p.IsProbe {
		// libwebrtc generates FEC before the sender fills in abs-send-time and
		// the transport-wide sequence number, and its FlexFEC receiver zeroes
		// both in media packets before recovery. Protect them as zeros too, or
		// recovered packets get wrong bytes there, and in the payload when the
		// header lengths in a group differ. The media is already written.
		// A send-side BWE interceptor writes transport-cc in WriteRTP, when
		// TransportWideExtID is zero, so zero the negotiated ID as well.
		zeroHeaderExtension(p.Header, p.AbsSendTimeExtID)
		zeroHeaderExtension(p.Header, p.TransportWideExtID)
		if p.FECTransportWideExtID != p.TransportWideExtID {
			zeroHeaderExtension(p.Header, p.FECTransportWideExtID)
		}
		repair := p.FEC.Encode(p.Header, p.Payload, p.IsKeyFrame)
		sent, payloadBytes, rtpBytes := 0, 0, 0
		for i := range repair {
			packet := &repair[i]
			fec := PacketFactory.Get().(*Packet)
			*fec = Packet{
				Header:             &packet.Header,
				Payload:            packet.Payload,
				AbsSendTimeExtID:   p.AbsSendTimeExtID,
				TransportWideExtID: p.TransportWideExtID,
				WriteStream:        p.WriteStream,
				ProbeClusterId:     p.ProbeClusterId,
			}
			// Include extension space in BWE/probe accounting before patching.
			if fec.AbsSendTimeExtID != 0 {
				_ = fec.Header.SetExtension(fec.AbsSendTimeExtID, []byte{0, 0, 0})
			}
			if fec.TransportWideExtID != 0 {
				_ = fec.Header.SetExtension(fec.TransportWideExtID, []byte{0, 0})
			}
			fec.HeaderSize = fec.Header.MarshalSize()
			// Charge repair bytes to this send's pacer budget. Repairs immediately
			// follow their media group even when there is a long media queue.
			n, fecErr := b.SendPacket(fec)
			written += n
			if fecErr == nil && n > 0 {
				sent++
				payloadBytes += len(packet.Payload)
				rtpBytes += packet.MarshalSize()
			}
		}
		p.FEC.RecordSent(sent, payloadBytes, rtpBytes)
	}
	return written, nil
}

func zeroHeaderExtension(header *rtp.Header, id uint8) {
	if id != 0 {
		clear(header.GetExtension(id))
	}
}

// patch just abs-send-time and transport-cc extensions if applicable
func (b *Base) patchRTPHeaderExtensions(p *Packet) error {
	sendingAt := mono.Now()
	if p.AbsSendTimeExtID != 0 {
		absSendTimeExt := rtp.AbsSendTimeExtension{
			Timestamp: uint64(mediatransportutil.ToNtpTime(sendingAt) >> 14),
		}
		if _, err := absSendTimeExt.MarshalTo(p.absSendTimeBuf[:]); err != nil {
			return err
		}

		if err := p.Header.SetExtension(p.AbsSendTimeExtID, p.absSendTimeBuf[:]); err != nil {
			return err
		}

		b.lastPacketSentAt.Store(sendingAt.UnixNano())
	}

	packetSize := p.HeaderSize + len(p.Payload) + int(p.Header.PaddingSize)
	if p.TransportWideExtID != 0 && b.bwe != nil {
		twccSN := b.bwe.RecordPacketSendAndGetSequenceNumber(
			sendingAt.UnixMicro(),
			packetSize,
			p.IsRTX,
			p.ProbeClusterId,
			p.IsProbe,
		)
		twccExt := rtp.TransportCCExtension{
			TransportSequence: twccSN,
		}
		if _, err := twccExt.MarshalTo(p.twccBuf[:]); err != nil {
			return err
		}

		if err := p.Header.SetExtension(p.TransportWideExtID, p.twccBuf[:]); err != nil {
			return err
		}

		b.lastPacketSentAt.Store(sendingAt.UnixNano())
	}

	b.ProbeObserver.RecordPacket(packetSize, p.IsRTX, p.ProbeClusterId, p.IsProbe)
	return nil
}

// ------------------------------------------------
