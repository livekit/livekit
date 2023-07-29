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
	"fmt"
)

type BitStreamWriter struct {
	buf       []byte
	pos       int
	bitOffset int // bit offset in the current byte
}

func NewBitStreamWriter(buf []byte) *BitStreamWriter {
	return &BitStreamWriter{buf: buf}
}

func (w *BitStreamWriter) RemainingBits() int {
	return (len(w.buf)-w.pos)*8 - w.bitOffset
}

func (w *BitStreamWriter) WriteBits(val uint64, bitCount int) error {
	if bitCount > w.RemainingBits() {
		return errors.New("insufficient space")
	}

	totalBits := bitCount

	// push bits to the highest bits of uint64
	val <<= 64 - bitCount

	buf := w.buf[w.pos:]

	// The first byte is relatively special; the bit offset to write to may put us
	// in the middle of the byte, and the total bit count to write may require we
	// save the bits at the end of the byte.
	remainingBitsInCurrentByte := 8 - w.bitOffset
	bitsInFirstByte := bitCount
	if bitsInFirstByte > remainingBitsInCurrentByte {
		bitsInFirstByte = remainingBitsInCurrentByte
	}

	buf[0] = w.writePartialByte(uint8(val>>56), bitsInFirstByte, buf[0], w.bitOffset)

	if bitCount <= remainingBitsInCurrentByte {
		// no bit left to write
		return w.consumeBits(totalBits)
	}

	// write the rest of the bits
	val <<= bitsInFirstByte
	buf = buf[1:]
	bitCount -= bitsInFirstByte
	for bitCount >= 8 {
		buf[0] = uint8(val >> 56)
		buf = buf[1:]
		val <<= 8
		bitCount -= 8
	}

	// write the last bits
	if bitCount > 0 {
		buf[0] = w.writePartialByte(uint8(val>>56), bitCount, buf[0], 0)
	}
	return w.consumeBits(totalBits)
}

func (w *BitStreamWriter) consumeBits(bitCount int) error {
	if bitCount > w.RemainingBits() {
		return errors.New("insufficient space")
	}

	w.pos += (w.bitOffset + bitCount) / 8
	w.bitOffset = (w.bitOffset + bitCount) % 8

	return nil
}

func (w *BitStreamWriter) writePartialByte(source uint8, sourceBitCount int, target uint8, targetBitOffset int) uint8 {
	// if !(targetBitOffset < 8 &&  sourceBitCount <= (8-targetBitOffset)) {
	// 	return fmt.Errorf("invalid argument, source %d, sourceBitCount %d, target %d, targetBitOffset %d", source, sourceBitCount, target, targetBitOffset)
	// }

	// generate mask for bits to overwrite, shift source bits to highest bits, then position to target bit offset
	mask := uint8(0xff<<(8-sourceBitCount)) >> uint8(targetBitOffset)

	// clear target bits and write source bits
	return (target & ^mask) | (source >> targetBitOffset)
}

func (w *BitStreamWriter) WriteNonSymmetric(val, numValues uint32) error {
	if !(val < numValues && numValues <= 1<<31) {
		return fmt.Errorf("invalid argument, val %d, numValues %d", val, numValues)
	}
	if numValues == 1 {
		// When there is only one possible value, it requires zero bits to store it.
		// But WriteBits doesn't support writing zero bits.
		return nil
	}

	countBits := bitwidth(numValues)
	numMinBitsValues := (uint32(1) << countBits) - numValues
	if val < numMinBitsValues {
		return w.WriteBits(uint64(val), countBits-1)
	} else {
		return w.WriteBits(uint64(val+numMinBitsValues), countBits)
	}
}

func SizeNonSymmetricBits(val, numValues uint32) int {
	countBits := bitwidth(numValues)
	numMinBitsValues := (uint32(1) << countBits) - numValues
	if val < numMinBitsValues {
		return countBits - 1
	} else {
		return countBits
	}
}
