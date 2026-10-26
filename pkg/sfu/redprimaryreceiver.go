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
	"go.uber.org/atomic"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/utils"
	"github.com/livekit/mediatransportutil/pkg/red"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

var _ REDTransformer = (*RedPrimaryReceiver)(nil)

type RedPrimaryReceiver struct {
	TrackReceiver
	downTrackSpreader *utils.DownTrackSpreader[TrackSender]
	logger            logger.Logger
	closed            atomic.Bool
	redPT             uint8

	depacketizer                      red.Depacketizer
	highestForwardedExtSequenceNumber uint64
	highestForwardedExtTimestamp      uint64

	// forwarded packet, reused since ForwardRTP runs on one goroutine and
	// down tracks do not keep the packet past WriteRTP
	sendExtPkt buffer.ExtPacket
}

func NewRedPrimaryReceiver(receiver TrackReceiver, dsp utils.DownTrackSpreaderParams) REDTransformer {
	return &RedPrimaryReceiver{
		TrackReceiver:     receiver,
		downTrackSpreader: utils.NewDownTrackSpreader[TrackSender](dsp),
		logger:            dsp.Logger,
		redPT:             uint8(receiver.Codec().PayloadType),
	}
}

func (r *RedPrimaryReceiver) SetLBThreshold(lbThreshold int) {
	r.downTrackSpreader.SetThreshold(lbThreshold)
}

func (r *RedPrimaryReceiver) ForwardRTP(pkt *buffer.ExtPacket, spatialLayer int32) int32 {
	// extract primary payload from RED and forward to downtracks
	if r.downTrackSpreader.DownTrackCount() == 0 {
		return 0
	}

	if pkt.Packet.PayloadType != r.redPT {
		// forward non-red packet directly
		return utils.BroadcastRTP(r.downTrackSpreader, pkt, spatialLayer)
	}

	pkts, err := r.depacketizer.Depacketize(pkt.Packet)
	if err != nil {
		r.logger.Errorw("get encoding for red failed", err, "payloadtype", pkt.Packet.PayloadType)
		return 0
	}

	var writeCount int32
	for i, sendPkt := range pkts {
		pPkt := &r.sendExtPkt
		*pPkt = *pkt
		if i != len(pkts)-1 {
			// patch extended sequence number and time stamp for all but the last packet,
			// last packet is the primary payload
			pPkt.ExtSequenceNumber -= uint64(pkts[len(pkts)-1].SequenceNumber - pkts[i].SequenceNumber)
			pPkt.ExtTimestamp -= uint64(pkts[len(pkts)-1].Timestamp - pkts[i].Timestamp)
		}
		pPkt.Packet = sendPkt

		if r.highestForwardedExtTimestamp != 0 {
			if (pPkt.ExtSequenceNumber > r.highestForwardedExtSequenceNumber && pPkt.ExtTimestamp < r.highestForwardedExtTimestamp) ||
				(pPkt.ExtSequenceNumber < r.highestForwardedExtSequenceNumber && pPkt.ExtTimestamp > r.highestForwardedExtTimestamp) {
				r.logger.Warnw(
					"sequence number OR timestamp inversion, dropping", nil,
					"numPackets", len(pkts),
					"primaryIncomingSN", pkt.Packet.Header.SequenceNumber,
					"primaryIncomingTS", pkt.Packet.Header.Timestamp,
					"primaryExtractedSN", pkts[len(pkts)-1].SequenceNumber,
					"primaryExtractedTS", pkts[len(pkts)-1].Timestamp,
					"primaryESN", pkt.ExtSequenceNumber,
					"primaryETS", pkt.ExtTimestamp,
					"packetIndex", i,
					"packetExtractedSN", pkts[i].SequenceNumber,
					"packetESN", pPkt.ExtSequenceNumber,
					"packetExtractedTS", pkts[i].Timestamp,
					"packetETS", pPkt.ExtTimestamp,
					"redHeader", pkt.Packet.Payload[:min(len(pkt.Packet.Payload), 10)],
					"payloadSize", len(pkt.Packet.Payload),
					"highestFowardedExtSequenceNumber", r.highestForwardedExtSequenceNumber,
					"highestFowardedExtTimestamp", r.highestForwardedExtTimestamp,
				)
				continue // drop the packet which causes the inversion
			}
		}
		r.highestForwardedExtSequenceNumber = max(r.highestForwardedExtSequenceNumber, pPkt.ExtSequenceNumber)
		r.highestForwardedExtTimestamp = max(r.highestForwardedExtTimestamp, pPkt.ExtTimestamp)

		// not modify the ExtPacket.RawPacket here for performance since it is not used by the DownTrack,
		// otherwise it should be set to the correct value (marshal the primary rtp packet)
		writeCount += utils.BroadcastRTP(r.downTrackSpreader, pPkt, spatialLayer)
	}
	return writeCount
}

func (r *RedPrimaryReceiver) ForwardRTCPSenderReport(
	payloadType webrtc.PayloadType,
	layer int32,
	publisherSRData *livekit.RTCPSenderReportState,
) {
	r.downTrackSpreader.Broadcast(func(dt TrackSender) {
		_ = dt.HandleRTCPSenderReportData(payloadType, layer, publisherSRData)
	})
}

func (r *RedPrimaryReceiver) AddDownTrack(track TrackSender) error {
	if r.closed.Load() {
		return ErrReceiverClosed
	}

	if r.downTrackSpreader.HasDownTrack(track.SubscriberID()) {
		r.logger.Infow("subscriberID already exists, replacing downtrack", "subscriberID", track.SubscriberID())
	}

	if !r.downTrackSpreader.TryStore(track) {
		return ErrReceiverClosed
	}
	r.logger.Debugw("red primary receiver downtrack added", "subscriberID", track.SubscriberID())
	return nil
}

func (r *RedPrimaryReceiver) DeleteDownTrack(subscriberID livekit.ParticipantID) {
	if r.closed.Load() {
		return
	}

	r.downTrackSpreader.Free(subscriberID)
	r.logger.Debugw("red primary receiver downtrack deleted", "subscriberID", subscriberID)
}

func (r *RedPrimaryReceiver) GetDownTracks() []TrackSender {
	return r.downTrackSpreader.GetDownTracks()
}

func (r *RedPrimaryReceiver) ResyncDownTracks() {
	r.downTrackSpreader.Broadcast(func(dt TrackSender) {
		dt.Resync()
	})
}

func (r *RedPrimaryReceiver) OnStreamRestart() {
	r.downTrackSpreader.Broadcast(func(dt TrackSender) {
		dt.ReceiverRestart(r)
	})
}

func (r *RedPrimaryReceiver) IsClosed() bool {
	return r.closed.Load()
}

func (r *RedPrimaryReceiver) CanClose() bool {
	return r.closed.Load() || r.downTrackSpreader.DownTrackCount() == 0
}

func (r *RedPrimaryReceiver) Close() {
	r.closed.Store(true)
	closeTrackSenders(r.downTrackSpreader.CloseAndGetDownTracks())
}

func (r *RedPrimaryReceiver) ReadRTP(buf []byte, layer uint8, esn uint64) (int, error) {
	n, err := r.TrackReceiver.ReadRTP(buf, layer, esn)
	if err != nil {
		return n, err
	}

	var pkt rtp.Packet
	pkt.Unmarshal(buf[:n])
	payload, err := red.ExtractPrimaryEncoding(pkt.Payload)
	if err != nil {
		return 0, err
	}
	pkt.Payload = payload

	return pkt.MarshalTo(buf)
}
