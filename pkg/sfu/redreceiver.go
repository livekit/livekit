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

package sfu

import (
	"encoding/binary"
	"fmt"

	"go.uber.org/atomic"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/mediatransportutil/pkg/bucket"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

const (
	maxRedCount   = 2
	mtuSize       = 1500
	maxRedPayload = 1 << 10 // fit into 10 bits length field

	// the RedReceiver is only for chrome / native webrtc now, we always negotiate opus payload to 111 with those clients,
	// so it is safe to use a fixed payload 111 here for performance(avoid encoding red blocks for each downtrack that
	// have a different opus payload type).
	opusPT    = 111
	opusRedPT = 63
)

type RedReceiver struct {
	TrackReceiver
	downTrackSpreader *DownTrackSpreader
	logger            logger.Logger
	closed            atomic.Bool
	pktBuff           [maxRedCount]*rtp.Packet
	redPayloadBuf     [mtuSize]byte
}

func NewRedReceiver(receiver TrackReceiver, dsp DownTrackSpreaderParams) *RedReceiver {
	return &RedReceiver{
		TrackReceiver:     receiver,
		downTrackSpreader: NewDownTrackSpreader(dsp),
		logger:            dsp.Logger,
	}
}

func (r *RedReceiver) ForwardRTP(pkt *buffer.ExtPacket, spatialLayer int32) int {
	// encode RED payload from primary payload and forward to downtracks
	if r.downTrackSpreader.DownTrackCount() == 0 {
		return 0
	}

	// fallback to primary codec if payload size exceeds redundant block length
	if len(pkt.Packet.Payload) >= maxRedPayload {
		return r.downTrackSpreader.Broadcast(func(dt TrackSender) {
			_ = dt.WriteRTP(pkt, spatialLayer)
		})
	}

	redLen, err := r.encodeRedForPrimary(pkt.Packet, r.redPayloadBuf[:])
	if err != nil {
		r.logger.Errorw("red encoding failed", err)
		return 0
	}

	pPkt := *pkt
	redRtpPacket := *pkt.Packet
	redRtpPacket.PayloadType = 63
	redRtpPacket.Payload = r.redPayloadBuf[:redLen]
	pPkt.Packet = &redRtpPacket

	// not modify the ExtPacket.RawPacket here for performance since it is not used by the DownTrack,
	// otherwise it should be set to the correct value (marshal the primary rtp packet)
	return r.downTrackSpreader.Broadcast(func(dt TrackSender) {
		_ = dt.WriteRTP(&pPkt, spatialLayer)
	})
}

func (r *RedReceiver) ForwardRTCPSenderReport(
	payloadType webrtc.PayloadType,
	layer int32,
	publisherSRData *livekit.RTCPSenderReportState,
) {
	r.downTrackSpreader.Broadcast(func(dt TrackSender) {
		_ = dt.HandleRTCPSenderReportData(payloadType, layer, publisherSRData)
	})
}

func (r *RedReceiver) AddDownTrack(track TrackSender) error {
	if r.closed.Load() {
		return ErrReceiverClosed
	}

	if r.downTrackSpreader.HasDownTrack(track.SubscriberID()) {
		r.logger.Infow("subscriberID already exists, replacing downtrack", "subscriberID", track.SubscriberID())
	}

	r.downTrackSpreader.Store(track)
	r.logger.Debugw("red receiver downtrack added", "subscriberID", track.SubscriberID())
	return nil
}

func (r *RedReceiver) DeleteDownTrack(subscriberID livekit.ParticipantID) {
	if r.closed.Load() {
		return
	}

	r.downTrackSpreader.Free(subscriberID)
	r.logger.Debugw("red receiver downtrack deleted", "subscriberID", subscriberID)
}

func (r *RedReceiver) GetDownTracks() []TrackSender {
	return r.downTrackSpreader.GetDownTracks()
}

func (r *RedReceiver) ResyncDownTracks() {
	r.downTrackSpreader.Broadcast(func(dt TrackSender) {
		dt.Resync()
	})
}

func (r *RedReceiver) CanClose() bool {
	return r.closed.Load() || r.downTrackSpreader.DownTrackCount() == 0
}

func (r *RedReceiver) IsClosed() bool {
	return r.closed.Load()
}

func (r *RedReceiver) Close() {
	r.closed.Store(true)
	closeTrackSenders(r.downTrackSpreader.ResetAndGetDownTracks())
}

func (r *RedReceiver) ReadRTP(buf []byte, layer uint8, esn uint64) (int, error) {
	// red encoding doesn't support nack
	return 0, bucket.ErrPacketMismatch
}

func (r *RedReceiver) encodeRedForPrimary(pkt *rtp.Packet, redPayload []byte) (int, error) {
	redLength := len(r.pktBuff)
	redPkts := make([]*rtp.Packet, 0, redLength+1)
	lastNilPkt := -1
	for i := redLength - 1; i >= 0; i-- {
		if r.pktBuff[i] == nil {
			lastNilPkt = i
			break
		}

	}

	for _, prev := range r.pktBuff[lastNilPkt+1:] {
		if pkt.SequenceNumber == prev.SequenceNumber ||
			(pkt.SequenceNumber-prev.SequenceNumber) > uint16(redLength) ||
			(pkt.Timestamp-prev.Timestamp) >= (1<<14) {
			continue
		}
		redPkts = append(redPkts, prev)
	}

	// insert primary packet in history buffer
	// NOTE: packet is copied from retransmission buffer and used in forwarding path. So, not making another
	// copy here and just maintaining pointer to the packet as the forwarding path should not alter the packet.
	for i := redLength - 1; i >= 0; i-- {
		if r.pktBuff[i] == nil || // history is empty
			pkt.SequenceNumber-r.pktBuff[i].SequenceNumber < (1<<15) { // received packet has more recent sequence number
			// age out older ones
			for j := 0; j < i; j++ {
				r.pktBuff[j] = r.pktBuff[j+1]
			}
			r.pktBuff[i] = pkt
			break
		}
	}

	return encodeRedForPrimary(redPkts, pkt, redPayload)
}

func encodeRedForPrimary(redPkts []*rtp.Packet, primary *rtp.Packet, redPayload []byte) (int, error) {
	payloadSize := len(primary.Payload) + 1
	for _, p := range redPkts {
		payloadSize += len(p.Payload) + 4
	}

	// if required payload size is larger than the redPayload buffer, encode the primary packet only
	if payloadSize > len(redPayload) {
		redPkts = redPkts[:0]
	}

	var index int
	for _, p := range redPkts {
		/* RED payload https://datatracker.ietf.org/doc/html/rfc2198#section-3
			0                   1                    2                   3
		    0 1 2 3 4 5 6 7 8 9 0 1 2 3  4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
		   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
		   |F|   block PT  |  timestamp offset         |   block length    |
		   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
		   F: 1 bit First bit in header indicates whether another header block
		       follows.  If 1 further header blocks follow, if 0 this is the
		       last header block.
		*/
		header := uint32(0x80 | uint8(opusPT))
		header <<= 14
		header |= (primary.Timestamp - p.Timestamp) & 0x3FFF
		header <<= 10
		header |= uint32(len(p.Payload)) & 0x3FF
		binary.BigEndian.PutUint32(redPayload[index:], header)
		index += 4
	}
	// last block header
	redPayload[index] = uint8(opusPT)
	index++

	// append data blocks
	redPkts = append(redPkts, primary)
	for _, p := range redPkts {
		if copy(redPayload[index:], p.Payload) < len(p.Payload) {
			return 0, fmt.Errorf("red payload don't have enough space, needsize %d", len(p.Payload))
		}
		index += len(p.Payload)
	}
	return index, nil
}
