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

package datatrack

import (
	"encoding/binary"
	"errors"
	"fmt"
)

var (
	errHeaderSizeInsufficient = errors.New("data track packet header size insufficient")
	errBufferSizeInsufficient = errors.New("data track packet buffer size insufficient")
)

const (
	headerLength = 12

	versionShift = 5
	versionMask  = (1 << 3) - 1

	startFrameShift = 4
	startFrameMask  = (1 << 1) - 1

	endFrameShift = 3
	endFrameMask  = (1 << 1) - 1

	extensionsShift = 2
	extensionsMask  = (1 << 1) - 1

	handleOffset = 2
	handleLength = 2

	seqNumOffset = 4
	seqNumLength = 2

	frameNumOffset = 6
	frameNumLength = 2

	timestampOffset = 8
	timestampLength = 4
)

type extension struct {
	id   uint16
	data []byte
}

type Header struct {
	Version        uint8
	IsStartOfFrame bool
	IsEndOfFrame   bool
	HasExtensions  bool
	Handle         uint16
	SequenceNumber uint16
	FrameNumber    uint16
	Timestamp      uint32
	Extensions     []extension
}

/*
	┆* +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	┆*  0                   1                   2                   3
	┆*  0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
	┆* +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	┆* |V    |S|E|X| reserved          | handle                        |
	┆* +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	┆* | sequence number               | frame number                  |
	┆* +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	┆* |                           timestamp                           |
	┆* +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
*/

func (h *Header) Unmarshal(buf []byte) (int, error) {
	if len(buf) < headerLength {
		return 0, fmt.Errorf("%w: %d < %d", errHeaderSizeInsufficient, len(buf), headerLength)
	}

	h.Version = buf[0] >> versionShift & versionMask
	h.IsStartOfFrame = (buf[0] >> startFrameShift & startFrameMask) > 0
	h.IsEndOfFrame = (buf[0] >> endFrameShift & endFrameMask) > 0
	h.HasExtensions = (buf[0] >> extensionsShift & extensionsMask) > 0

	h.Handle = binary.BigEndian.Uint16(buf[handleOffset : handleOffset+handleLength])
	h.SequenceNumber = binary.BigEndian.Uint16(buf[seqNumOffset : seqNumOffset+seqNumLength])
	h.FrameNumber = binary.BigEndian.Uint16(buf[frameNumOffset : frameNumOffset+frameNumLength])
	h.Timestamp = binary.BigEndian.Uint32(buf[timestampOffset : timestampOffset+timestampLength])

	if h.HasExtensions {
		// DT-TODO
	}

	return headerLength, nil
}

func (h *Header) MarshalSize() int {
	// DT-TODO: if extensions are there, need to add extension length
	return headerLength
}

func (h *Header) MarshalTo(buf []byte) (int, error) {
	if len(buf) < headerLength {
		return 0, fmt.Errorf("%w: %d < %d", errHeaderSizeInsufficient, len(buf), headerLength)
	}

	buf[0] = h.Version << versionShift
	if h.IsStartOfFrame {
		buf[0] |= (1 << startFrameShift)
	}
	if h.IsEndOfFrame {
		buf[0] |= (1 << endFrameShift)
	}
	if h.HasExtensions {
		buf[0] |= (1 << extensionsShift)
	}

	if h.HasExtensions {
		// DT-TODO marshal extensions
	}

	binary.BigEndian.PutUint16(buf[handleOffset:handleOffset+handleLength], h.Handle)
	binary.BigEndian.PutUint16(buf[seqNumOffset:seqNumOffset+seqNumLength], h.SequenceNumber)
	binary.BigEndian.PutUint16(buf[frameNumOffset:frameNumOffset+frameNumLength], h.FrameNumber)
	binary.BigEndian.PutUint32(buf[timestampOffset:timestampOffset+timestampLength], h.Timestamp)

	return headerLength, nil
}

// ----------------------------------------------------

type Packet struct {
	Header
	Payload []byte
}

func (p *Packet) Unmarshal(buf []byte) error {
	hdrSize, err := p.Header.Unmarshal(buf)
	if err != nil {
		return err
	}

	p.Payload = buf[hdrSize:]
	return nil
}

func (p *Packet) Marshal() ([]byte, error) {
	buf := make([]byte, p.Header.MarshalSize()+len(p.Payload))
	if err := p.MarshalTo(buf); err != nil {
		return nil, err
	}

	return buf, nil
}

func (p *Packet) MarshalTo(buf []byte) error {
	size := p.Header.MarshalSize() + len(p.Payload)
	if len(buf) < size {
		return fmt.Errorf("%w: %d < %d", errBufferSizeInsufficient, len(buf), size)
	}

	hdrSize, err := p.Header.MarshalTo(buf)
	if err != nil {
		return err
	}

	copy(buf[hdrSize:], p.Payload)
	return nil
}
