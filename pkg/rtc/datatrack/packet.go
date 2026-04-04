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
	errExtensionSizeTooBig       = errors.New("extension size is too big")
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

	extensionIDLength   = 1
	extensionSizeLength = 1
)

type Extension struct {
	id   uint8
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
	┆* |V    |S|F|X| reserved          | handle                        |
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
	┆* | Extension ID  | Extension size| Extension payload             |
	┆* +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+

	End of all extensions
	┆* +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	|* padded to 4 byte boundary if aggregate of `Extensions Size`     |
	|* field and all extensions do not end on a 4 byte boundary        |
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
		extensionsSize := (binary.BigEndian.Uint16(buf[extensionsSizeOffset:extensionsSizeOffset+extensionsSizeLength])+1)*4 - extensionsSizeLength
		hdrSize += extensionsSizeLength

		extensionHeaderSize := extensionIDLength + extensionSizeLength
		remainingSize := int(extensionsSize)
		idx := extensionsSizeOffset + extensionsSizeLength
		for remainingSize != 0 {
			// read extension header
			if len(buf[idx:]) < extensionIDLength || remainingSize < extensionIDLength {
				return 0, fmt.Errorf("%w: %d/%d < %d", errExtensionSizeInsufficient, remainingSize, len(buf[idx:]), extensionIDLength)
			}
			id := buf[idx]
			if id == 0 {
				// end of extensions, padding has started
				hdrSize += remainingSize
				break
			}

			if len(buf[idx+1:]) < extensionSizeLength || remainingSize < extensionSizeLength {
				return 0, fmt.Errorf("%w: %d/%d < %d", errExtensionSizeInsufficient, remainingSize, len(buf[idx:]), extensionSizeLength)
			}
			size := int(buf[idx+1])

			remainingSize -= extensionHeaderSize
			idx += extensionHeaderSize
			hdrSize += extensionHeaderSize

			// read extension data
			if len(buf[idx:]) < size || remainingSize < size {
				return 0, fmt.Errorf("%w: %d/%d < %d", errExtensionSizeInsufficient, remainingSize, len(buf[idx:]), size)
			}
			h.Extensions = append(h.Extensions, Extension{id: id, data: buf[idx : idx+size]})

			remainingSize -= size
			idx += size
			hdrSize += size
		}
		h.ExtensionsSize = extensionsSize - uint16(remainingSize)
	}

	return hdrSize, nil
}

func (h *Header) MarshalSize() int {
	extensionsSize := 0
	if h.HasExtensions {
		extensionsSize += extensionsSizeLength
		for _, ext := range h.Extensions {
			extensionsSize += len(ext.data) + extensionIDLength + extensionSizeLength
		}
	}

	return headerLength + (extensionsSize+3)/4*4
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
		extensionsSize := (extensionsSizeLength + h.ExtensionsSize + 3) / 4 * 4
		binary.BigEndian.PutUint16(buf[extensionsSizeOffset:extensionsSizeOffset+extensionsSizeLength], (extensionsSize/4)-1)
		hdrSize += extensionsSizeLength

		addedSize := 0
		idx := extensionsSizeOffset + extensionsSizeLength
		for _, ext := range h.Extensions {
			buf[idx] = ext.id
			if len(ext.data) > 255 {
				return 0, fmt.Errorf("%w: %d > 255", errExtensionSizeTooBig, len(ext.data))
			}
			buf[idx+extensionIDLength] = byte(len(ext.data))
			copy(buf[idx+extensionIDLength+extensionSizeLength:], ext.data)

			extSize := len(ext.data) + extensionIDLength + extensionSizeLength
			idx += extSize
			hdrSize += extSize
			addedSize += extSize
		}

		paddingSize := extensionsSize - extensionsSizeLength - uint16(addedSize)
		for i := range paddingSize {
			buf[idx+int(i)] = 0
		}
		idx += int(paddingSize)
		hdrSize += int(paddingSize)
	}

	return hdrSize, nil
}

func (h *Header) AddExtension(ext Extension) {
	for i, existingExt := range h.Extensions {
		if existingExt.id == ext.id {
			h.ExtensionsSize -= uint16(len(existingExt.data) + extensionIDLength + extensionSizeLength)
			h.Extensions[i].data = ext.data
			h.ExtensionsSize += uint16(len(h.Extensions[i].data) + extensionIDLength + extensionSizeLength)
			return
		}
	}

	h.Extensions = append(h.Extensions, ext)
	h.ExtensionsSize += uint16(len(ext.data) + extensionIDLength + extensionSizeLength)
	h.HasExtensions = true
}

func (h *Header) GetExtension(id uint8) (Extension, error) {
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
