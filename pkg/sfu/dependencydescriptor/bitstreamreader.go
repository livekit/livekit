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

package dependencydescriptor

import (
	"errors"
	"io"
)

type BitStreamReader struct {
	buf           []byte
	pos           int
	remainingBits int
}

func NewBitStreamReader(buf []byte) *BitStreamReader {
	return &BitStreamReader{buf: buf, remainingBits: len(buf) * 8}
}

func (b *BitStreamReader) RemainingBits() int {
	return b.remainingBits
}

// Reads `bits` from the bitstream. `bits` must be in range [0, 64].
// Returns an unsigned integer in range [0, 2^bits - 1].
// On failure sets `BitstreamReader` into the failure state and returns 0.
func (b *BitStreamReader) ReadBits(bits int) (uint64, error) {
	if bits < 0 || bits > 64 {
		return 0, errors.New("invalid number of bits, expected 0-64")
	}

	if b.remainingBits < bits {
		b.remainingBits -= bits
		return 0, io.EOF
	}

	remainingBitsInFirstByte := b.remainingBits % 8
	b.remainingBits -= bits
	if bits < remainingBitsInFirstByte {
		// Reading fewer bits than what's left in the current byte, just
		// return the portion of this byte that is needed.
		offset := remainingBitsInFirstByte - bits
		return uint64((b.buf[b.pos] >> offset) & ((1 << bits) - 1)), nil
	}
	var result uint64
	if remainingBitsInFirstByte > 0 {
		// Read all bits that were left in the current byte and consume that byte.
		bits -= remainingBitsInFirstByte
		mask := byte((1 << remainingBitsInFirstByte) - 1)
		result = uint64(b.buf[b.pos]&mask) << bits
		b.pos++
	}
	// Read as many full bytes as we can.
	for bits >= 8 {
		bits -= 8
		result |= uint64(b.buf[b.pos]) << bits
		b.pos++
	}

	// Whatever is left to read is smaller than a byte, so grab just the needed
	// bits and shift them into the lowest bits.
	if bits > 0 {
		result |= uint64(b.buf[b.pos] >> (8 - bits))
	}
	return result, nil
}

func (b *BitStreamReader) ReadBool() (bool, error) {
	val, err := b.ReadBits(1)
	return val != 0, err
}

func (b *BitStreamReader) Ok() bool {
	return b.remainingBits >= 0
}

func (b *BitStreamReader) Invalidate() {
	b.remainingBits = -1
}

// Reads value in range [0, `num_values` - 1].
// This encoding is similar to ReadBits(val, Ceil(Log2(num_values)),
// but reduces wastage incurred when encoding non-power of two value ranges
// Non symmetric values are encoded as:
// 1) n = bit_width(num_values)
// 2) k = (1 << n) - num_values
// Value v in range [0, k - 1] is encoded in (n-1) bits.
// Value v in range [k, num_values - 1] is encoded as (v+k) in n bits.
// https://aomediacodec.github.io/av1-spec/#nsn
func (b *BitStreamReader) ReadNonSymmetric(numValues uint32) (uint32, error) {
	if numValues >= (uint32(1) << 31) {
		return 0, errors.New("invalid number of values, expected 0-2^31")
	}

	width := bitwidth(numValues)
	numMinBitsValues := (uint32(1) << width) - numValues

	val, err := b.ReadBits(width - 1)
	if err != nil {
		return 0, err
	}
	if val < uint64(numMinBitsValues) {
		return uint32(val), nil
	}
	bit, err := b.ReadBits(1)
	if err != nil {
		return 0, err
	}
	return uint32((val << 1) + bit - uint64(numMinBitsValues)), nil
}

func (b *BitStreamReader) BytesRead() int {
	if b.remainingBits%8 > 0 {
		return b.pos + 1
	}
	return b.pos
}

func bitwidth(n uint32) int {
	var w int
	for n != 0 {
		n >>= 1
		w++
	}
	return w
}
