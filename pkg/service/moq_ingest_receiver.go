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

package service

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"sync/atomic"

	"github.com/livekit/protocol/codecs/mime"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
)

const (
	moqIngestClockRate   = 90000
	moqIngestPayloadType = 102
	moqIngestRTPMTU      = 1188
)

type moqIngestFrame struct {
	sequence uint64
	payload  []byte
	keyFrame bool
}

type moqIngestReceiver struct {
	*sfu.ReceiverBase

	config config.MoQConfig
	logger logger.Logger
	buff   *buffer.Buffer

	payloader codecs.H264Payloader

	ssrc          uint32
	rtpSequence   uint16
	rtpTimestamp  uint32
	timestampStep uint32

	frames chan moqIngestFrame
	done   chan struct{}

	lock         sync.Mutex
	closed       bool
	nextSequence uint64
	dropUntilIDR bool

	pliRequests atomic.Uint64
}

func newMoQIngestReceiver(
	trackID livekit.TrackID,
	trackName string,
	width uint32,
	height uint32,
	fps uint32,
	config config.MoQConfig,
	lgr logger.Logger,
) (*moqIngestReceiver, error) {
	if fps == 0 {
		fps = 30
	}
	ssrc, err := randomUint32()
	if err != nil {
		return nil, err
	}
	if ssrc == 0 {
		ssrc = 1
	}

	codec := moqIngestH264Codec()
	ti := &livekit.TrackInfo{
		Sid:      string(trackID),
		Type:     livekit.TrackType_VIDEO,
		Name:     trackName,
		Width:    width,
		Height:   height,
		Source:   livekit.TrackSource_CAMERA,
		Stream:   "camera",
		MimeType: mime.MimeTypeH264.String(),
		Codecs: []*livekit.SimulcastCodecInfo{
			{
				Cid:            string(trackID),
				MimeType:       mime.MimeTypeH264.String(),
				VideoLayerMode: livekit.VideoLayer_ONE_SPATIAL_LAYER_PER_STREAM,
				Layers: []*livekit.VideoLayer{
					{
						Quality:      livekit.VideoQuality_HIGH,
						Width:        width,
						Height:       height,
						Bitrate:      1_500_000,
						SpatialLayer: 0,
					},
				},
			},
		},
	}

	r := &moqIngestReceiver{
		config:        config,
		logger:        lgr.WithValues("trackID", trackID, "trackName", trackName),
		ssrc:          ssrc,
		timestampStep: uint32(moqIngestClockRate / fps),
		frames:        make(chan moqIngestFrame, config.IngestQueueSize),
		done:          make(chan struct{}),
		dropUntilIDR:  true,
	}
	r.ReceiverBase = sfu.NewReceiverBase(
		sfu.ReceiverBaseParams{
			TrackID:       trackID,
			StreamID:      "camera",
			Kind:          webrtc.RTPCodecTypeVideo,
			Codec:         codec,
			Logger:        r.logger,
			IsSelfClosing: false,
		},
		ti,
		sfu.ReceiverCodecStateNormal,
	)

	r.buff = buffer.NewBuffer(ssrc, config.IngestQueueSize, 0)
	r.buff.SetLogger(r.logger)
	if err := r.buff.Bind(webrtc.RTPParameters{Codecs: []webrtc.RTPCodecParameters{codec}}, codec.RTPCodecCapability, 0); err != nil {
		return nil, err
	}
	r.buff.OnRtcpFeedback(func(pkts []rtcp.Packet) {
		for _, pkt := range pkts {
			switch pkt.(type) {
			case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
				r.pliRequests.Add(1)
				r.logger.Debugw("moq ingest keyframe requested")
			}
		}
	})
	r.AddBuffer(r.buff, 0)
	r.StartBuffer(r.buff, 0)

	go r.run()
	return r, nil
}

func moqIngestH264Codec() webrtc.RTPCodecParameters {
	return webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:     webrtc.MimeTypeH264,
			ClockRate:    moqIngestClockRate,
			SDPFmtpLine:  "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
			RTCPFeedback: []webrtc.RTCPFeedback{{Type: "nack"}, {Type: "nack", Parameter: "pli"}, {Type: "goog-remb"}},
		},
		PayloadType: moqIngestPayloadType,
	}
}

func (r *moqIngestReceiver) EnqueueAccessUnit(sequence uint64, payload []byte) bool {
	if len(payload) == 0 || len(payload) > r.config.IngestMaxFrameBytes {
		return false
	}
	frame := moqIngestFrame{
		sequence: sequence,
		payload:  cloneBytes(payload),
		keyFrame: h264AnnexBHasIDR(payload),
	}

	select {
	case <-r.done:
		return false
	case r.frames <- frame:
		return true
	default:
	}

	if !frame.keyFrame {
		return false
	}
	select {
	case <-r.done:
		return false
	case <-r.frames:
	default:
	}
	select {
	case <-r.done:
		return false
	case r.frames <- frame:
		return true
	default:
		return false
	}
}

func (r *moqIngestReceiver) SetNextSequence(next uint64) {
	r.lock.Lock()
	if next > r.nextSequence {
		r.nextSequence = next
	}
	r.dropUntilIDR = true
	r.lock.Unlock()
}

func (r *moqIngestReceiver) NextSequence() uint64 {
	r.lock.Lock()
	defer r.lock.Unlock()
	return r.nextSequence
}

func (r *moqIngestReceiver) Close() {
	r.lock.Lock()
	if r.closed {
		r.lock.Unlock()
		return
	}
	r.closed = true
	close(r.done)
	r.lock.Unlock()
	r.ReceiverBase.Close("moq-ingest-close", true)
}

func (r *moqIngestReceiver) run() {
	for {
		select {
		case <-r.done:
			return
		case frame := <-r.frames:
			if err := r.writeAccessUnit(frame); err != nil && !errors.Is(err, io.EOF) {
				r.logger.Debugw("could not write moq ingest access unit", "error", err)
			}
		}
	}
}

func (r *moqIngestReceiver) writeAccessUnit(frame moqIngestFrame) error {
	r.lock.Lock()
	if r.closed {
		r.lock.Unlock()
		return io.EOF
	}
	if frame.sequence < r.nextSequence {
		r.lock.Unlock()
		return nil
	}
	if r.dropUntilIDR && !frame.keyFrame {
		r.nextSequence = frame.sequence + 1
		r.lock.Unlock()
		return nil
	}
	if frame.sequence > r.nextSequence {
		r.nextSequence = frame.sequence
	}
	r.nextSequence = frame.sequence + 1
	if frame.keyFrame {
		r.dropUntilIDR = false
	}

	rtpTime := r.rtpTimestamp
	r.rtpTimestamp += r.timestampStep
	payloads := r.payloader.Payload(moqIngestRTPMTU, frame.payload)
	packets := make([][]byte, 0, len(payloads))
	for i, payload := range payloads {
		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    moqIngestPayloadType,
				SequenceNumber: r.rtpSequence,
				Timestamp:      rtpTime,
				SSRC:           r.ssrc,
				Marker:         i == len(payloads)-1,
			},
			Payload: payload,
		}
		r.rtpSequence++
		raw, err := pkt.Marshal()
		if err != nil {
			r.lock.Unlock()
			return err
		}
		packets = append(packets, raw)
	}
	r.lock.Unlock()

	for _, raw := range packets {
		if _, err := r.buff.Write(raw); err != nil {
			return err
		}
	}
	return nil
}

func h264AnnexBHasIDR(payload []byte) bool {
	for _, nalu := range splitAnnexBNALUs(payload) {
		if len(nalu) != 0 && nalu[0]&0x1f == 5 {
			return true
		}
	}
	return false
}

func randomUint32() (uint32, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(b[:]), nil
}

var _ sfu.TrackReceiver = (*moqIngestReceiver)(nil)
