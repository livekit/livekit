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

package testutils

import (
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
)

// -----------------------------------------------------------

type TestExtPacketParams struct {
	Marker         bool
	IsKeyFrame     bool
	PayloadType    uint8
	SequenceNumber uint16
	SNCycles       int
	Timestamp      uint32
	TSCycles       int
	SSRC           uint32
	PayloadSize    int
	PaddingSize    byte
	ArrivalTime    time.Time
	VideoLayer     buffer.VideoLayer
	IsOutOfOrder   bool
}

// -----------------------------------------------------------

func GetTestExtPacket(params *TestExtPacketParams) (*buffer.ExtPacket, error) {
	packet := rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			Padding:        params.PaddingSize != 0,
			Marker:         params.Marker,
			PayloadType:    params.PayloadType,
			SequenceNumber: params.SequenceNumber,
			Timestamp:      params.Timestamp,
			SSRC:           params.SSRC,
		},
		Payload:     make([]byte, params.PayloadSize),
		PaddingSize: params.PaddingSize,
	}

	raw, err := packet.Marshal()
	if err != nil {
		return nil, err
	}

	ep := &buffer.ExtPacket{
		VideoLayer:        params.VideoLayer,
		ExtSequenceNumber: uint64(params.SNCycles<<16) + uint64(params.SequenceNumber),
		ExtTimestamp:      uint64(params.TSCycles<<32) + uint64(params.Timestamp),
		Arrival:           params.ArrivalTime.UnixNano(),
		Packet:            &packet,
		KeyFrame:          params.IsKeyFrame,
		RawPacket:         raw,
		IsOutOfOrder:      params.IsOutOfOrder,
	}

	return ep, nil
}

// --------------------------------------

func GetTestExtPacketVP8(params *TestExtPacketParams, vp8 *buffer.VP8) (*buffer.ExtPacket, error) {
	ep, err := GetTestExtPacket(params)
	if err != nil {
		return nil, err
	}

	ep.KeyFrame = vp8.IsKeyFrame
	ep.Payload = *vp8
	if ep.DependencyDescriptor == nil {
		ep.Temporal = int32(vp8.TID)
	}
	return ep, nil
}

// --------------------------------------

var TestVP8Codec = webrtc.RTPCodecCapability{
	MimeType:  "video/vp8",
	ClockRate: 90000,
}

var TestOpusCodec = webrtc.RTPCodecCapability{
	MimeType:  "audio/opus",
	ClockRate: 48000,
}

// --------------------------------------
