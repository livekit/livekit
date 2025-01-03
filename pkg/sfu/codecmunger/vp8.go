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

package codecmunger

import (
	"github.com/elliotchance/orderedmap/v2"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
)

const (
	missingPictureIdsThreshold  = 50
	droppedPictureIdsThreshold  = 20
	exemptedPictureIdsThreshold = 20
)

// -----------------------------------------------------------

type VP8 struct {
	logger logger.Logger

	pictureIdWrapHandler VP8PictureIdWrapHandler
	extLastPictureId     int32
	pictureIdOffset      int32
	pictureIdUsed        bool
	lastTl0PicIdx        uint8
	tl0PicIdxOffset      uint8
	tl0PicIdxUsed        bool
	tidUsed              bool
	lastKeyIdx           uint8
	keyIdxOffset         uint8
	keyIdxUsed           bool

	missingPictureIds  *orderedmap.OrderedMap[int32, int32]
	droppedPictureIds  *orderedmap.OrderedMap[int32, bool]
	exemptedPictureIds *orderedmap.OrderedMap[int32, bool]
}

func NewVP8(logger logger.Logger) *VP8 {
	return &VP8{
		logger:             logger,
		missingPictureIds:  orderedmap.NewOrderedMap[int32, int32](),
		droppedPictureIds:  orderedmap.NewOrderedMap[int32, bool](),
		exemptedPictureIds: orderedmap.NewOrderedMap[int32, bool](),
	}
}

func NewVP8FromNull(cm CodecMunger, logger logger.Logger) *VP8 {
	v := NewVP8(logger)
	v.SeedState(cm.(*Null).GetSeededState())
	return v
}

func (v *VP8) GetState() interface{} {
	return &livekit.VP8MungerState{
		ExtLastPictureId: v.extLastPictureId,
		PictureIdUsed:    v.pictureIdUsed,
		LastTl0PicIdx:    uint32(v.lastTl0PicIdx),
		Tl0PicIdxUsed:    v.tl0PicIdxUsed,
		TidUsed:          v.tidUsed,
		LastKeyIdx:       uint32(v.lastKeyIdx),
		KeyIdxUsed:       v.keyIdxUsed,
	}
}

func (v *VP8) SeedState(seed interface{}) {
	switch cm := seed.(type) {
	case *livekit.RTPForwarderState_Vp8Munger:
		state := cm.Vp8Munger
		v.extLastPictureId = state.ExtLastPictureId
		v.pictureIdUsed = state.PictureIdUsed
		v.lastTl0PicIdx = uint8(state.LastTl0PicIdx)
		v.tl0PicIdxUsed = state.Tl0PicIdxUsed
		v.tidUsed = state.TidUsed
		v.lastKeyIdx = uint8(state.LastKeyIdx)
		v.keyIdxUsed = state.KeyIdxUsed
	}
}

func (v *VP8) SetLast(extPkt *buffer.ExtPacket) {
	vp8, ok := extPkt.Payload.(buffer.VP8)
	if !ok {
		return
	}

	v.pictureIdUsed = vp8.I
	if v.pictureIdUsed {
		v.pictureIdWrapHandler.Init(int32(vp8.PictureID)-1, vp8.M)
		v.extLastPictureId = int32(vp8.PictureID)
	}

	v.tl0PicIdxUsed = vp8.L
	if v.tl0PicIdxUsed {
		v.lastTl0PicIdx = vp8.TL0PICIDX
	}

	v.tidUsed = vp8.T

	v.keyIdxUsed = vp8.K
	if v.keyIdxUsed {
		v.lastKeyIdx = vp8.KEYIDX
	}
}

func (v *VP8) UpdateOffsets(extPkt *buffer.ExtPacket) {
	vp8, ok := extPkt.Payload.(buffer.VP8)
	if !ok {
		return
	}

	if v.pictureIdUsed {
		v.pictureIdWrapHandler.Init(int32(vp8.PictureID)-1, vp8.M)
		v.pictureIdOffset = int32(vp8.PictureID) - v.extLastPictureId - 1
	}

	if v.tl0PicIdxUsed {
		v.tl0PicIdxOffset = vp8.TL0PICIDX - v.lastTl0PicIdx - 1
	}

	if v.keyIdxUsed {
		v.keyIdxOffset = (vp8.KEYIDX - v.lastKeyIdx - 1) & 0x1f
	}

	// clear picture id caches on layer switch
	v.missingPictureIds = orderedmap.NewOrderedMap[int32, int32]()
	v.droppedPictureIds = orderedmap.NewOrderedMap[int32, bool]()
	v.exemptedPictureIds = orderedmap.NewOrderedMap[int32, bool]()
}

func (v *VP8) UpdateAndGet(extPkt *buffer.ExtPacket, snOutOfOrder bool, snHasGap bool, maxTemporalLayer int32) (int, []byte, error) {
	vp8, ok := extPkt.Payload.(buffer.VP8)
	if !ok {
		return 0, nil, ErrNotVP8
	}

	extPictureId := v.pictureIdWrapHandler.Unwrap(vp8.PictureID, vp8.M)

	// if out-of-order, look up missing picture id cache
	if snOutOfOrder {
		pictureIdOffset, ok := v.missingPictureIds.Get(extPictureId)
		if !ok {
			return 0, nil, ErrOutOfOrderVP8PictureIdCacheMiss
		}

		// the out-of-order picture id cannot be deleted from the cache
		// as there could more than one packet in a picture and more
		// than one packet of a picture could come out-of-order.
		// To prevent picture id cache from growing, it is truncated
		// when it reaches a certain size.

		mungedPictureId := uint16((extPictureId - pictureIdOffset) & 0x7fff)
		vp8Packet := &buffer.VP8{
			FirstByte:  vp8.FirstByte,
			I:          vp8.I,
			M:          mungedPictureId > 127,
			PictureID:  mungedPictureId,
			L:          vp8.L,
			TL0PICIDX:  vp8.TL0PICIDX - v.tl0PicIdxOffset,
			T:          vp8.T,
			TID:        vp8.TID,
			Y:          vp8.Y,
			K:          vp8.K,
			KEYIDX:     vp8.KEYIDX - v.keyIdxOffset,
			IsKeyFrame: vp8.IsKeyFrame,
			HeaderSize: vp8.HeaderSize + buffer.VPxPictureIdSizeDiff(mungedPictureId > 127, vp8.M),
		}
		vp8HeaderBytes, err := vp8Packet.Marshal()
		if err != nil {
			return 0, nil, err
		}
		return vp8.HeaderSize, vp8HeaderBytes, nil
	}

	prevMaxPictureId := v.pictureIdWrapHandler.MaxPictureId()
	v.pictureIdWrapHandler.UpdateMaxPictureId(extPictureId, vp8.M)

	// if there is a gap in sequence number, record possible pictures that
	// the missing packets can belong to in missing picture id cache.
	// The missing picture cache should contain the previous picture id
	// and the current picture id and all the intervening pictures.
	// This is to handle a scenario as follows
	//   o Packet 10 -> Picture ID 10
	//   o Packet 11 -> missing
	//   o Packet 12 -> Picture ID 11
	// In this case, Packet 11 could belong to either Picture ID 10 (last packet of that picture)
	// or Picture ID 11 (first packet of the current picture). Although in this simple case,
	// it is possible to deduce that (for example by looking at previous packet's RTP marker
	// and check if that was the last packet of Picture 10), it could get complicated when
	// the gap is larger.
	if snHasGap {
		for lostPictureId := prevMaxPictureId; lostPictureId <= extPictureId; lostPictureId++ {
			// Record missing only if picture id was not dropped. This is to avoid a subsequent packet of dropped frame going through.
			// A sequence like this
			//   o Packet 10 - Picture 11 - TID that should be dropped
			//   o Packet 11 - missing - belongs to Picture 11 still
			//   o Packet 12 - Picture 12 - will be reported as GAP, so missing picture id mapping will be set up for Picture 11 also.
			//   o Next packet - Packet 11 - this will use the wrong offset from missing pictures cache
			_, ok := v.droppedPictureIds.Get(lostPictureId)
			if !ok {
				v.missingPictureIds.Set(lostPictureId, v.pictureIdOffset)
			}
		}

		// trim cache if necessary
		for v.missingPictureIds.Len() > missingPictureIdsThreshold {
			el := v.missingPictureIds.Front()
			v.missingPictureIds.Delete(el.Key)
		}

		// if there is a gap, packet is forwarded irrespective of temporal layer as it cannot be determined
		// which layer the missing packets belong to. A layer could have multiple packets. So, keep track
		// of pictures that are forwarded even though they will be filtered out based on temporal layer
		// requirements. That allows forwarding of the complete picture.
		if extPkt.Temporal > maxTemporalLayer {
			v.exemptedPictureIds.Set(extPictureId, true)
			// trim cache if necessary
			for v.exemptedPictureIds.Len() > exemptedPictureIdsThreshold {
				el := v.exemptedPictureIds.Front()
				v.exemptedPictureIds.Delete(el.Key)
			}
		}
	} else {
		if extPkt.Temporal > maxTemporalLayer {
			// drop only if not exempted
			_, ok := v.exemptedPictureIds.Get(extPictureId)
			if !ok {
				// adjust only once per picture as a picture could have multiple packets
				if vp8.I && prevMaxPictureId != extPictureId {
					// keep track of dropped picture ids so that they do not get into the missing picture cache
					v.droppedPictureIds.Set(extPictureId, true)
					// trim cache if necessary
					for v.droppedPictureIds.Len() > droppedPictureIdsThreshold {
						el := v.droppedPictureIds.Front()
						v.droppedPictureIds.Delete(el.Key)
					}

					v.pictureIdOffset += 1
				}
				return 0, nil, ErrFilteredVP8TemporalLayer
			}
		}
	}

	// in-order incoming sequence number, may or may not be contiguous.
	// In the case of loss (i.e. incoming sequence number is not contiguous),
	// forward even if it is a filtered layer. With temporal scalability,
	// it is unclear if the current packet should be dropped if it is not
	// contiguous. Hence, forward anything that is not contiguous.
	// Reference: http://www.rtcbits.com/2017/04/howto-implement-temporal-scalability.html
	extMungedPictureId := extPictureId - v.pictureIdOffset
	mungedPictureId := uint16(extMungedPictureId & 0x7fff)
	mungedTl0PicIdx := vp8.TL0PICIDX - v.tl0PicIdxOffset
	mungedKeyIdx := (vp8.KEYIDX - v.keyIdxOffset) & 0x1f

	v.extLastPictureId = extMungedPictureId
	v.lastTl0PicIdx = mungedTl0PicIdx
	v.lastKeyIdx = mungedKeyIdx

	vp8Packet := &buffer.VP8{
		FirstByte:  vp8.FirstByte,
		I:          vp8.I,
		M:          mungedPictureId > 127,
		PictureID:  mungedPictureId,
		L:          vp8.L,
		TL0PICIDX:  mungedTl0PicIdx,
		T:          vp8.T,
		TID:        vp8.TID,
		Y:          vp8.Y,
		K:          vp8.K,
		KEYIDX:     mungedKeyIdx,
		IsKeyFrame: vp8.IsKeyFrame,
		HeaderSize: vp8.HeaderSize + buffer.VPxPictureIdSizeDiff(mungedPictureId > 127, vp8.M),
	}
	vp8HeaderBytes, err := vp8Packet.Marshal()
	if err != nil {
		return 0, nil, err
	}
	return vp8.HeaderSize, vp8HeaderBytes, nil
}

func (v *VP8) UpdateAndGetPadding(newPicture bool) ([]byte, error) {
	offset := 0
	if newPicture {
		offset = 1
	}

	headerSize := 1
	if v.pictureIdUsed || v.tl0PicIdxUsed || v.tidUsed || v.keyIdxUsed {
		headerSize += 1
	}

	extPictureId := v.extLastPictureId
	if v.pictureIdUsed {
		extPictureId = v.extLastPictureId + int32(offset)
		v.extLastPictureId = extPictureId
		v.pictureIdOffset -= int32(offset)
		if (extPictureId & 0x7fff) > 127 {
			headerSize += 2
		} else {
			headerSize += 1
		}
	}
	pictureId := uint16(extPictureId & 0x7fff)

	tl0PicIdx := uint8(0)
	if v.tl0PicIdxUsed {
		tl0PicIdx = v.lastTl0PicIdx + uint8(offset)
		v.lastTl0PicIdx = tl0PicIdx
		v.tl0PicIdxOffset -= uint8(offset)
		headerSize += 1
	}

	if v.tidUsed || v.keyIdxUsed {
		headerSize += 1
	}

	keyIdx := uint8(0)
	if v.keyIdxUsed {
		keyIdx = (v.lastKeyIdx + uint8(offset)) & 0x1f
		v.lastKeyIdx = keyIdx
		v.keyIdxOffset -= uint8(offset)
	}

	vp8Packet := &buffer.VP8{
		FirstByte:  0x10, // partition 0, start of VP8 Partition, reference frame
		I:          v.pictureIdUsed,
		M:          pictureId > 127,
		PictureID:  pictureId,
		L:          v.tl0PicIdxUsed,
		TL0PICIDX:  tl0PicIdx,
		T:          v.tidUsed,
		TID:        0,
		Y:          true,
		K:          v.keyIdxUsed,
		KEYIDX:     keyIdx,
		IsKeyFrame: true,
		HeaderSize: headerSize,
	}
	return vp8Packet.Marshal()
}

// for testing only
func (v *VP8) PictureIdOffset(extPictureId int32) (int32, bool) {
	return v.missingPictureIds.Get(extPictureId)
}

// -----------------------------

// VP8PictureIdWrapHandler
func isWrapping7Bit(val1 int32, val2 int32) bool {
	return val2 < val1 && (val1-val2) > (1<<6)
}

func isWrapping15Bit(val1 int32, val2 int32) bool {
	return val2 < val1 && (val1-val2) > (1<<14)
}

type VP8PictureIdWrapHandler struct {
	maxPictureId int32
	maxMBit      bool
	totalWrap    int32
	lastWrap     int32
}

func (v *VP8PictureIdWrapHandler) Init(extPictureId int32, mBit bool) {
	v.maxPictureId = extPictureId
	v.maxMBit = mBit
	v.totalWrap = 0
	v.lastWrap = 0
}

func (v *VP8PictureIdWrapHandler) MaxPictureId() int32 {
	return v.maxPictureId
}

// unwrap picture id and update the maxPictureId. return unwrapped value
func (v *VP8PictureIdWrapHandler) Unwrap(pictureId uint16, mBit bool) int32 {
	//
	// VP8 Picture ID is specified very flexibly.
	//
	// Reference: https://datatracker.ietf.org/doc/html/draft-ietf-payload-vp8
	//
	// Quoting from the RFC
	// ----------------------------
	// PictureID:  7 or 15 bits (shown left and right, respectively, in
	//    Figure 2) not including the M bit.  This is a running index of
	//    the frames, which MAY start at a random value, MUST increase by
	//    1 for each subsequent frame, and MUST wrap to 0 after reaching
	//    the maximum ID (all bits set).  The 7 or 15 bits of the
	//    PictureID go from most significant to least significant,
	//    beginning with the first bit after the M bit.  The sender
	//    chooses a 7 or 15 bit index and sets the M bit accordingly.
	//    The receiver MUST NOT assume that the number of bits in
	//    PictureID stay the same through the session.  Having sent a
	//    7-bit PictureID with all bits set to 1, the sender may either
	//    wrap the PictureID to 0, or extend to 15 bits and continue
	//    incrementing
	// ----------------------------
	//
	// While in practice, senders may not switch between modes indiscriminately,
	// it is possible that small picture ids are sent in 7 bits and then switch
	// to 15 bits. But, to ensure correctness, this code keeps track of how much
	// quantity has wrapped and uses that to figure out if the incoming picture id
	// is newer OR out-of-order.
	//
	maxPictureId := v.maxPictureId
	// maxPictureId can be -1 at the start
	if maxPictureId > 0 {
		if v.maxMBit {
			maxPictureId = v.maxPictureId & 0x7fff
		} else {
			maxPictureId = v.maxPictureId & 0x7f
		}
	}

	var newPictureId int32
	if mBit {
		newPictureId = int32(pictureId & 0x7fff)
	} else {
		newPictureId = int32(pictureId & 0x7f)
	}

	//
	// if the new picture id is too far ahead of max, i.e. more than half of last wrap,
	// it is out-of-order, unwrap backwards
	//
	if v.totalWrap > 0 {
		if (v.maxPictureId + (v.lastWrap >> 1)) < (newPictureId + v.totalWrap) {
			return newPictureId + v.totalWrap - v.lastWrap
		}
	}

	//
	// check for wrap around based on mode of previous picture id.
	// There are three cases here
	//   1. Wrapping from 15-bit -> 8-bit (32767 -> 0)
	//   2. Wrapping from 15-bit -> 15-bit (32767 -> 0)
	//   3. Wrapping from 8-bit -> 8-bit (127 -> 0)
	// In all cases, looking at the mode of previous picture id will
	// ensure that we are calculating the wrap properly.
	//
	wrap := int32(0)
	if v.maxMBit {
		if isWrapping15Bit(maxPictureId, newPictureId) {
			wrap = 1 << 15
		}
	} else {
		if isWrapping7Bit(maxPictureId, newPictureId) {
			wrap = 1 << 7
		}
	}

	v.totalWrap += wrap
	if wrap != 0 {
		v.lastWrap = wrap
	}
	newPictureId += v.totalWrap

	return newPictureId
}

func (v *VP8PictureIdWrapHandler) UpdateMaxPictureId(extPictureId int32, mBit bool) {
	v.maxPictureId = extPictureId
	v.maxMBit = mBit
}
