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
	"math/rand"
	"time"
)

func GenerateRawDataPackets(handle uint16, seqNum uint16, frameNum uint16, numFrames int, frameSize int, frameDuration time.Duration) [][]byte {
	if seqNum == 0 {
		seqNum = uint16(rand.Intn(256) + 1)
	}
	if frameNum == 0 {
		frameNum = uint16(rand.Intn(256) + 1)
	}
	timestamp := uint32(rand.Intn(1024))

	packetsPerFrame := (frameSize + 255) / 256 // using 256 bytes of payload per packet
	if packetsPerFrame == 0 {
		return nil
	}
	numPackets := packetsPerFrame * numFrames
	rawPackets := make([][]byte, 0, numPackets)
	for range numFrames {
		remainingSize := frameSize
		for packetIdx := range packetsPerFrame {
			payloadSize := min(remainingSize, 256)
			payload := make([]byte, payloadSize)
			for i := range len(payload) {
				payload[i] = byte(255 - i)
			}
			packet := &Packet{
				Header: Header{
					Version:        0,
					IsStartOfFrame: packetIdx == 0,
					IsFinalOfFrame: packetIdx == packetsPerFrame-1,
					Handle:         handle,
					SequenceNumber: seqNum,
					FrameNumber:    frameNum,
					Timestamp:      timestamp,
				},
				Payload: payload,
			}
			if extParticipantSid, err := NewExtensionParticipantSid("test_participant"); err == nil {
				if ext, err := extParticipantSid.Marshal(); err == nil {
					packet.AddExtension(ext)
				}
			}
			rawPacket, err := packet.Marshal()
			if err == nil {
				rawPackets = append(rawPackets, rawPacket)
			}
			seqNum++
			remainingSize -= payloadSize
		}
		frameNum++
		timestamp += uint32(90000 * frameDuration.Seconds())
	}

	return rawPackets
}
