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
	dd "github.com/livekit/livekit-server/pkg/sfu/rtpextension/dependencydescriptor"
	"github.com/livekit/mediatransportutil/pkg/codec"
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
			PaddingSize:    params.PaddingSize,
		},
		Payload: make([]byte, params.PayloadSize),
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
		IsKeyFrame:        params.IsKeyFrame,
		RawPacket:         raw,
		IsOutOfOrder:      params.IsOutOfOrder,
	}

	return ep, nil
}

// --------------------------------------

func GetTestExtPacketVP8(params *TestExtPacketParams, vp8 *codec.VP8) (*buffer.ExtPacket, error) {
	ep, err := GetTestExtPacket(params)
	if err != nil {
		return nil, err
	}

	ep.IsKeyFrame = vp8.IsKeyFrame
	ep.Payload = *vp8
	if ep.DependencyDescriptor == nil {
		ep.Temporal = int32(vp8.TID)
	}
	return ep, nil
}

// --------------------------------------

type TestDDParams struct {
	FirstPacketInFrame bool
	ExtFrameNum        uint64
	TemporalID         int
	// decode target indications as one character per decode target: S, R, D or -
	DTIs          string
	FrameDiffs    []int
	DecodeTargets []buffer.DependencyDescriptorDecodeTarget
}

// GetTestExtPacketVP8DD returns a VP8 packet with a dependency descriptor and the
// minimized VP8 payload descriptor that libwebrtc sends with it: no TID, no Y bit and
// no picture ID.
func GetTestExtPacketVP8DD(params *TestExtPacketParams, ddParams *TestDDParams) (*buffer.ExtPacket, error) {
	ep, err := GetTestExtPacketVP8(params, &codec.VP8{S: ddParams.FirstPacketInFrame, IsKeyFrame: params.IsKeyFrame})
	if err != nil {
		return nil, err
	}

	dtis := make([]dd.DecodeTargetIndication, 0, len(ddParams.DTIs))
	for _, c := range ddParams.DTIs {
		switch c {
		case 'S':
			dtis = append(dtis, dd.DecodeTargetSwitch)
		case 'R':
			dtis = append(dtis, dd.DecodeTargetRequired)
		case 'D':
			dtis = append(dtis, dd.DecodeTargetDiscardable)
		default:
			dtis = append(dtis, dd.DecodeTargetNotPresent)
		}
	}
	ep.Temporal = int32(ddParams.TemporalID)
	ep.DependencyDescriptor = &buffer.ExtDependencyDescriptor{
		Descriptor: &dd.DependencyDescriptor{
			FirstPacketInFrame: ddParams.FirstPacketInFrame,
			LastPacketInFrame:  params.Marker,
			FrameDependencies: &dd.FrameDependencyTemplate{
				TemporalId:              ddParams.TemporalID,
				DecodeTargetIndications: dtis,
				FrameDiffs:              ddParams.FrameDiffs,
			},
		},
		DecodeTargets: ddParams.DecodeTargets,
		ExtFrameNum:   ddParams.ExtFrameNum,
	}
	return ep, nil
}

// TestVP8DecodeTargets are the decode targets of libwebrtc's three layer VP8 structure,
// sorted from high to low.
var TestVP8DecodeTargets = []buffer.DependencyDescriptorDecodeTarget{
	{Target: 2, Layer: buffer.VideoLayer{Temporal: 2}},
	{Target: 1, Layer: buffer.VideoLayer{Temporal: 1}},
	{Target: 0, Layer: buffer.VideoLayer{Temporal: 0}},
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
