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
	errHeaderSizeInsufficient    = errors.New("data track packet header size insufficient")
	errBufferSizeInsufficient    = errors.New("data track packet buffer size insufficient")
	errExtensionSizeInsufficient = errors.New("data track packet extension size insufficient")
	errExtensionNotFound         = errors.New("data track packet extension not found")
)

const (
	headerLength = 12

	versionShift = 5
	versionMask  = (1 << 3) - 1

	startOfFrameShift = 4
	startOfFrameMask  = (1 << 1) - 1

	finalOfFrameShift = 3
	finalOfFrameMask  = (1 << 1) - 1

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

	extensionsSizeOffset = headerLength
	extensionsSizeLength = 2

	extensionIDLength   = 2
	extensionSizeLength = 2
)

type Extension struct {
	id   uint16
	data []byte
}

type Header struct {
	Version        uint8
	IsStartOfFrame bool
	IsFinalOfFrame bool
	HasExtensions  bool
	Handle         uint16
	SequenceNumber uint16
	FrameNumber    uint16
	Timestamp      uint32
	ExtensionsSize uint16
	Extensions     []Extension
}

/*
	┆* +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	┆*  0                   1                   2                   3
	┆*  0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
	┆* +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	┆* |V    |F|L|X| reserved          | handle                        |
	┆* +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	┆* | sequence number               | frame number                  |
	┆* +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	┆* |                           timestamp                           |
	┆* +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	|* Extensions Size if X=1          | Extensions...                 |
	┆* +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+

	Each extension
	┆* +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	┆*  0                   1                   2                   3
	┆*  0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
	┆* +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	┆* | Extension ID                  | Extension size                |
	┆* +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	|* Extension payload (padded to 4 byte boundary)                   |
	┆* +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
*/

func (h *Header) Unmarshal(buf []byte) (int, error) {
	if len(buf) < headerLength {
		return 0, fmt.Errorf("%w: %d < %d", errHeaderSizeInsufficient, len(buf), headerLength)
	}

	hdrSize := headerLength
	h.Version = buf[0] >> versionShift & versionMask
	h.IsStartOfFrame = (buf[0] >> startOfFrameShift & startOfFrameMask) > 0
	h.IsFinalOfFrame = (buf[0] >> finalOfFrameShift & finalOfFrameMask) > 0
	h.HasExtensions = (buf[0] >> extensionsShift & extensionsMask) > 0

	h.Handle = binary.BigEndian.Uint16(buf[handleOffset : handleOffset+handleLength])
	h.SequenceNumber = binary.BigEndian.Uint16(buf[seqNumOffset : seqNumOffset+seqNumLength])
	h.FrameNumber = binary.BigEndian.Uint16(buf[frameNumOffset : frameNumOffset+frameNumLength])
	h.Timestamp = binary.BigEndian.Uint32(buf[timestampOffset : timestampOffset+timestampLength])

	if h.HasExtensions {
		h.ExtensionsSize = (binary.BigEndian.Uint16(buf[extensionsSizeOffset:extensionsSizeOffset+extensionsSizeLength]) + 1) * 4
		hdrSize += extensionsSizeLength

		remainingSize := int(h.ExtensionsSize)
		idx := extensionsSizeOffset + extensionsSizeLength
		for remainingSize != 0 {
			if len(buf[idx:]) < 4 || remainingSize < 4 {
				return 0, fmt.Errorf("%w: %d/%d < %d", errExtensionSizeInsufficient, remainingSize, len(buf[idx:]), 4)
			}

			id := binary.BigEndian.Uint16(buf[idx : idx+2])
			size := int(binary.BigEndian.Uint16(buf[idx+2 : idx+4]))
			remainingSize -= 4
			idx += 4
			hdrSize += 4

			if len(buf[idx:]) < size || remainingSize < size {
				return 0, fmt.Errorf("%w: %d/%d < %d", errExtensionSizeInsufficient, remainingSize, len(buf[idx:]), size)
			}
			h.Extensions = append(h.Extensions, Extension{id: id, data: buf[idx : idx+size]})

			size = ((size + 3) / 4) * 4
			remainingSize -= size
			idx += size
			hdrSize += size
		}
	}

	return hdrSize, nil
}

func (h *Header) MarshalSize() int {
	size := headerLength
	if h.HasExtensions {
		size += 2 // extensions size field
		for _, ext := range h.Extensions {
			size += ((len(ext.data)+3)/4)*4 + 2 /* extension ID field */ + 2 /* extension length field */
		}
	}
	return size
}

func (h *Header) MarshalTo(buf []byte) (int, error) {
	if len(buf) < headerLength {
		return 0, fmt.Errorf("%w: %d < %d", errHeaderSizeInsufficient, len(buf), headerLength)
	}

	hdrSize := headerLength
	buf[0] = h.Version << versionShift
	if h.IsStartOfFrame {
		buf[0] |= (1 << startOfFrameShift)
	}
	if h.IsFinalOfFrame {
		buf[0] |= (1 << finalOfFrameShift)
	}
	if h.HasExtensions {
		buf[0] |= (1 << extensionsShift)
	}

	binary.BigEndian.PutUint16(buf[handleOffset:handleOffset+handleLength], h.Handle)
	binary.BigEndian.PutUint16(buf[seqNumOffset:seqNumOffset+seqNumLength], h.SequenceNumber)
	binary.BigEndian.PutUint16(buf[frameNumOffset:frameNumOffset+frameNumLength], h.FrameNumber)
	binary.BigEndian.PutUint32(buf[timestampOffset:timestampOffset+timestampLength], h.Timestamp)

	if h.HasExtensions {
		binary.BigEndian.PutUint16(buf[extensionsSizeOffset:extensionsSizeOffset+extensionsSizeLength], (h.ExtensionsSize/4)-1)
		hdrSize += extensionsSizeLength

		idx := extensionsSizeOffset + extensionsSizeLength
		for _, ext := range h.Extensions {
			binary.BigEndian.PutUint16(buf[idx:idx+extensionIDLength], ext.id)
			binary.BigEndian.PutUint16(buf[idx+extensionIDLength:idx+extensionIDLength+extensionSizeLength], uint16(len(ext.data)))
			copy(buf[idx+extensionIDLength+extensionSizeLength:], ext.data)

			idx += ((len(ext.data)+3)/4)*4 + 2 /* extension ID field */ + 2     /* extension length field */
			hdrSize += ((len(ext.data)+3)/4)*4 + 2 /* extension ID field */ + 2 /* extension length field */
		}
	}

	return hdrSize, nil
}

func (h *Header) AddExtension(ext Extension) {
	for i, existingExt := range h.Extensions {
		if existingExt.id == ext.id {
			h.ExtensionsSize -= uint16((len(existingExt.data)+3)/4*4 + 2 /* extension ID field */ + 2 /* extension length field */)
			h.Extensions[i].data = ext.data
			h.ExtensionsSize += uint16((len(h.Extensions[i].data)+3)/4*4 + 2 /* extension ID field */ + 2 /* extension length field */)
			return
		}
	}

	h.Extensions = append(h.Extensions, ext)
	h.ExtensionsSize += uint16((len(ext.data)+3)/4*4 + 2 /* extension ID field */ + 2 /* extension length field */)
	h.HasExtensions = true
}

func (h *Header) GetExtension(id uint16) (Extension, error) {
	for _, ext := range h.Extensions {
		if ext.id == id {
			return ext, nil
		}
	}
	return Extension{}, fmt.Errorf("%w, id: %d", errExtensionNotFound, id)
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
