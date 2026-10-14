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

package codecmunger

import (
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/mediatransportutil/pkg/codec"
	"github.com/livekit/protocol/codecs/mime"
	"github.com/livekit/protocol/logger"
)

// frames with a remembered decision, behind the highest frame number
const ddTemporalFrameWindow = 64

// DDTemporal filters the temporal layers of a simulcast stream with the dependency
// descriptor, for any codec: it leaves the payload unchanged. Every packet names its frame
// and temporal layer, so the munger decides once per frame and applies that decision to all
// packets of the frame, a late one included. For VP8, libwebrtc sends a one byte payload
// descriptor with the dependency descriptor, so there is no picture ID to munge; the
// forwarder uses the VP8 munger for a stream that keeps picture IDs.
type DDTemporal struct {
	logger logger.Logger
	isVP8  bool // padding needs a VP8 payload descriptor

	initialized   bool
	highestFrame  uint64
	decidedMask   uint64 // bit i: frame highestFrame - i has a decision
	forwardedMask uint64 // bit i: frame highestFrame - i is forwarded
}

func NewDDTemporal(mimeType mime.MimeType, logger logger.Logger) *DDTemporal {
	return &DDTemporal{
		logger: logger,
		isVP8:  mimeType == mime.MimeTypeVP8,
	}
}

func (v *DDTemporal) GetState() any {
	return nil
}

func (v *DDTemporal) SeedState(_state any) {
}

func (v *DDTemporal) SetLast(_extPkt *buffer.ExtPacket) {
	v.initialized = false
}

func (v *DDTemporal) UpdateOffsets(_extPkt *buffer.ExtPacket) {
	// a layer switch starts a stream with its own frame numbers
	v.initialized = false
}

func (v *DDTemporal) UpdateAndGet(extPkt *buffer.ExtPacket, snOutOfOrder bool, _snHasGap bool, maxTemporalLayer int32) (int, [MaxHeaderSize]byte, int, error) {
	var hdr [MaxHeaderSize]byte
	ddwdt := extPkt.DependencyDescriptor
	if ddwdt == nil {
		return 0, hdr, 0, nil
	}

	frame := ddwdt.ExtFrameNum
	isForwarded, ok := v.decision(frame)
	if !ok {
		isForwarded = extPkt.Temporal <= maxTemporalLayer
		if !v.record(frame, isForwarded) {
			// a late packet of a frame that is too old to have a decision
			isForwarded = false
		}
	}

	switch {
	case isForwarded:
		return 0, hdr, 0, nil
	case snOutOfOrder:
		return 0, hdr, 0, ErrFilteredOutOfOrder
	default:
		return 0, hdr, 0, ErrFilteredTemporalLayer
	}
}

func (v *DDTemporal) UpdateAndGetPadding(_newPicture bool) ([]byte, error) {
	if !v.isVP8 {
		return nil, nil
	}

	// the one byte VP8 payload descriptor of a key frame that starts a partition
	vp8Packet := &codec.VP8{
		FirstByte:  0x10,
		IsKeyFrame: true,
		HeaderSize: 1,
	}
	return vp8Packet.Marshal()
}

func (v *DDTemporal) decision(frame uint64) (isForwarded bool, ok bool) {
	if !v.initialized || frame > v.highestFrame || v.highestFrame-frame >= ddTemporalFrameWindow {
		return false, false
	}
	bit := uint64(1) << (v.highestFrame - frame)
	return v.forwardedMask&bit != 0, v.decidedMask&bit != 0
}

func (v *DDTemporal) record(frame uint64, isForwarded bool) bool {
	switch {
	case !v.initialized || frame >= v.highestFrame+ddTemporalFrameWindow:
		v.initialized = true
		v.highestFrame = frame
		v.decidedMask = 0
		v.forwardedMask = 0

	case frame > v.highestFrame:
		v.decidedMask <<= frame - v.highestFrame
		v.forwardedMask <<= frame - v.highestFrame
		v.highestFrame = frame

	case v.highestFrame-frame >= ddTemporalFrameWindow:
		return false
	}

	bit := uint64(1) << (v.highestFrame - frame)
	v.decidedMask |= bit
	if isForwarded {
		v.forwardedMask |= bit
	}
	return true
}
