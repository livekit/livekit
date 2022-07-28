package sfu

import (
	"github.com/elliotchance/orderedmap"

	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
)

//
// VP8 munger
//
type TranslationParamsVP8 struct {
	Header *buffer.VP8
}

type VP8MungerState struct {
	ExtLastPictureId int32
	PictureIdUsed    int
	LastTl0PicIdx    uint8
	Tl0PicIdxUsed    int
	TidUsed          int
	LastKeyIdx       uint8
	KeyIdxUsed       int
}

type VP8MungerParams struct {
	pictureIdWrapHandler VP8PictureIdWrapHandler
	extLastPictureId     int32
	pictureIdOffset      int32
	pictureIdUsed        int
	lastTl0PicIdx        uint8
	tl0PicIdxOffset      uint8
	tl0PicIdxUsed        int
	tidUsed              int
	lastKeyIdx           uint8
	keyIdxOffset         uint8
	keyIdxUsed           int

	missingPictureIds    *orderedmap.OrderedMap
	lastDroppedPictureId int32
}

type VP8Munger struct {
	logger logger.Logger

	VP8MungerParams
}

func NewVP8Munger(logger logger.Logger) *VP8Munger {
	return &VP8Munger{
		logger: logger,
		VP8MungerParams: VP8MungerParams{
			missingPictureIds:    orderedmap.NewOrderedMap(),
			lastDroppedPictureId: -1,
		},
	}
}

func (v *VP8Munger) GetLast() VP8MungerState {
	return VP8MungerState{
		ExtLastPictureId: v.extLastPictureId,
		PictureIdUsed:    v.pictureIdUsed,
		LastTl0PicIdx:    v.lastTl0PicIdx,
		Tl0PicIdxUsed:    v.tl0PicIdxUsed,
		TidUsed:          v.tidUsed,
		LastKeyIdx:       v.lastKeyIdx,
		KeyIdxUsed:       v.keyIdxUsed,
	}
}

func (v *VP8Munger) SeedLast(state VP8MungerState) {
	v.extLastPictureId = state.ExtLastPictureId
	v.pictureIdUsed = state.PictureIdUsed
	v.lastTl0PicIdx = state.LastTl0PicIdx
	v.tl0PicIdxUsed = state.Tl0PicIdxUsed
	v.tidUsed = state.TidUsed
	v.lastKeyIdx = state.LastKeyIdx
	v.keyIdxUsed = state.KeyIdxUsed
}

func (v *VP8Munger) SetLast(extPkt *buffer.ExtPacket) {
	vp8, ok := extPkt.Payload.(buffer.VP8)
	if !ok {
		return
	}

	v.pictureIdUsed = vp8.PictureIDPresent
	if v.pictureIdUsed == 1 {
		v.pictureIdWrapHandler.Init(int32(vp8.PictureID)-1, vp8.MBit)
		v.extLastPictureId = int32(vp8.PictureID)
	}

	v.tl0PicIdxUsed = vp8.TL0PICIDXPresent
	if v.tl0PicIdxUsed == 1 {
		v.lastTl0PicIdx = vp8.TL0PICIDX
	}

	v.tidUsed = vp8.TIDPresent

	v.keyIdxUsed = vp8.KEYIDXPresent
	if v.keyIdxUsed == 1 {
		v.lastKeyIdx = vp8.KEYIDX
	}

	v.lastDroppedPictureId = -1
}

func (v *VP8Munger) UpdateOffsets(extPkt *buffer.ExtPacket) {
	vp8, ok := extPkt.Payload.(buffer.VP8)
	if !ok {
		return
	}

	if v.pictureIdUsed == 1 {
		v.pictureIdWrapHandler.Init(int32(vp8.PictureID)-1, vp8.MBit)
		v.pictureIdOffset = int32(vp8.PictureID) - v.extLastPictureId - 1
	}

	if v.tl0PicIdxUsed == 1 {
		v.tl0PicIdxOffset = vp8.TL0PICIDX - v.lastTl0PicIdx - 1
	}

	if v.keyIdxUsed == 1 {
		v.keyIdxOffset = (vp8.KEYIDX - v.lastKeyIdx - 1) & 0x1f
	}

	// clear missing picture ids on layer switch
	v.missingPictureIds = orderedmap.NewOrderedMap()

	v.lastDroppedPictureId = -1
}

func (v *VP8Munger) UpdateAndGet(extPkt *buffer.ExtPacket, ordering SequenceNumberOrdering, maxTemporalLayer int32) (*TranslationParamsVP8, error) {
	vp8, ok := extPkt.Payload.(buffer.VP8)
	if !ok {
		return nil, ErrNotVP8
	}

	extPictureId := v.pictureIdWrapHandler.Unwrap(vp8.PictureID, vp8.MBit)

	// if out-of-order, look up missing picture id cache
	if ordering == SequenceNumberOrderingOutOfOrder {
		value, ok := v.missingPictureIds.Get(extPictureId)
		if !ok {
			return nil, ErrOutOfOrderVP8PictureIdCacheMiss
		}
		pictureIdOffset := value.(int32)

		// the out-of-order picture id cannot be deleted from the cache
		// as there could more than one packet in a picture and more
		// than one packet of a picture could come out-of-order.
		// To prevent picture id cache from growing, it is truncated
		// when it reaches a certain size.

		mungedPictureId := uint16((extPictureId - pictureIdOffset) & 0x7fff)
		vp8Packet := &buffer.VP8{
			FirstByte:        vp8.FirstByte,
			PictureIDPresent: vp8.PictureIDPresent,
			PictureID:        mungedPictureId,
			MBit:             mungedPictureId > 127,
			TL0PICIDXPresent: vp8.TL0PICIDXPresent,
			TL0PICIDX:        vp8.TL0PICIDX - v.tl0PicIdxOffset,
			TIDPresent:       vp8.TIDPresent,
			TID:              vp8.TID,
			Y:                vp8.Y,
			KEYIDXPresent:    vp8.KEYIDXPresent,
			KEYIDX:           vp8.KEYIDX - v.keyIdxOffset,
			IsKeyFrame:       vp8.IsKeyFrame,
			HeaderSize:       vp8.HeaderSize + buffer.VP8PictureIdSizeDiff(mungedPictureId > 127, vp8.MBit),
		}
		return &TranslationParamsVP8{
			Header: vp8Packet,
		}, nil
	}

	prevMaxPictureId := v.pictureIdWrapHandler.MaxPictureId()
	v.pictureIdWrapHandler.UpdateMaxPictureId(extPictureId, vp8.MBit)

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
	if ordering == SequenceNumberOrderingGap {
		// can drop packet if it belongs to the last dropped picture.
		// Example:
		//   o Packet 10 - Picture 11 - TID that should be dropped
		//   o Packet 11 - missing
		//   o Packet 12 - Picture 11 - will be reported as GAP, but belongs to a picture that was dropped and hence can be dropped
		// If Packet 11 comes around, it will be reported as OUT_OF_ORDER, but the missing
		// picture id cache will not have an entry and hence will be dropped.
		if extPictureId == v.lastDroppedPictureId {
			return nil, ErrFilteredVP8TemporalLayer
		} else {
			for lostPictureId := prevMaxPictureId; lostPictureId <= extPictureId; lostPictureId++ {
				v.missingPictureIds.Set(lostPictureId, v.pictureIdOffset)
			}

			// trim cache if necessary
			for v.missingPictureIds.Len() > 50 {
				el := v.missingPictureIds.Front()
				v.missingPictureIds.Delete(el.Key)
			}
		}
	} else {
		if vp8.TIDPresent == 1 && vp8.TID > uint8(maxTemporalLayer) {
			// adjust only once per picture as a picture could have multiple packets
			if vp8.PictureIDPresent == 1 && prevMaxPictureId != extPictureId {
				v.lastDroppedPictureId = extPictureId
				v.pictureIdOffset += 1
			}
			return nil, ErrFilteredVP8TemporalLayer
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
		FirstByte:        vp8.FirstByte,
		PictureIDPresent: vp8.PictureIDPresent,
		PictureID:        mungedPictureId,
		MBit:             mungedPictureId > 127,
		TL0PICIDXPresent: vp8.TL0PICIDXPresent,
		TL0PICIDX:        mungedTl0PicIdx,
		TIDPresent:       vp8.TIDPresent,
		TID:              vp8.TID,
		Y:                vp8.Y,
		KEYIDXPresent:    vp8.KEYIDXPresent,
		KEYIDX:           mungedKeyIdx,
		IsKeyFrame:       vp8.IsKeyFrame,
		HeaderSize:       vp8.HeaderSize + buffer.VP8PictureIdSizeDiff(mungedPictureId > 127, vp8.MBit),
	}
	return &TranslationParamsVP8{
		Header: vp8Packet,
	}, nil
}

func (v *VP8Munger) UpdateAndGetPadding(newPicture bool) *buffer.VP8 {
	offset := 0
	if newPicture {
		offset = 1
	}

	headerSize := 1
	if (v.pictureIdUsed + v.tl0PicIdxUsed + v.tidUsed + v.keyIdxUsed) != 0 {
		headerSize += 1
	}

	extPictureId := v.extLastPictureId
	if v.pictureIdUsed == 1 {
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
	if v.tl0PicIdxUsed == 1 {
		tl0PicIdx = v.lastTl0PicIdx + uint8(offset)
		v.lastTl0PicIdx = tl0PicIdx
		v.tl0PicIdxOffset -= uint8(offset)
		headerSize += 1
	}

	if (v.tidUsed + v.keyIdxUsed) != 0 {
		headerSize += 1
	}

	keyIdx := uint8(0)
	if v.keyIdxUsed == 1 {
		keyIdx = (v.lastKeyIdx + uint8(offset)) & 0x1f
		v.lastKeyIdx = keyIdx
		v.keyIdxOffset -= uint8(offset)
	}

	vp8Packet := &buffer.VP8{
		FirstByte:        0x10, // partition 0, start of VP8 Partition, reference frame
		PictureIDPresent: v.pictureIdUsed,
		PictureID:        pictureId,
		MBit:             pictureId > 127,
		TL0PICIDXPresent: v.tl0PicIdxUsed,
		TL0PICIDX:        tl0PicIdx,
		TIDPresent:       v.tidUsed,
		TID:              0,
		Y:                1,
		KEYIDXPresent:    v.keyIdxUsed,
		KEYIDX:           keyIdx,
		IsKeyFrame:       true,
		HeaderSize:       headerSize,
	}
	return vp8Packet
}

// for testing only
func (v *VP8Munger) PictureIdOffset(extPictureId int32) (int32, bool) {
	value, ok := v.missingPictureIds.Get(extPictureId)
	return value.(int32), ok
}

// -----------------------------

//
// VP8PictureIdWrapHandler
//
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
