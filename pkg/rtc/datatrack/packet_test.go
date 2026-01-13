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
	"testing"

	"github.com/livekit/protocol/livekit"
	"github.com/stretchr/testify/require"
)

func TestPacket(t *testing.T) {
	t.Run("without extension", func(t *testing.T) {
		payload := make([]byte, 6)
		for i := range len(payload) {
			payload[i] = byte(255 - i)
		}
		packet := &Packet{
			Header: Header{
				Version:        0,
				IsStartOfFrame: true,
				IsFinalOfFrame: true,
				Handle:         3333,
				SequenceNumber: 6666,
				FrameNumber:    9999,
				Timestamp:      0xdeadbeef,
			},
			Payload: payload,
		}
		rawPacket, err := packet.Marshal()
		require.NoError(t, err)

		expectedRawPacket := []byte{
			0x18, 0x00, 0x0d, 0x05, 0x1a, 0x0a, 0x27, 0x0f,
			0xde, 0xad, 0xbe, 0xef, 0xff, 0xfe, 0xfd, 0xfc,
			0xfb, 0xfa,
		}
		require.Equal(t, expectedRawPacket, rawPacket)

		var unmarshaled Packet
		err = unmarshaled.Unmarshal(rawPacket)
		require.NoError(t, err)
		require.Equal(t, packet, &unmarshaled)
	})

	t.Run("with extension", func(t *testing.T) {
		payload := make([]byte, 4)
		for i := range len(payload) {
			payload[i] = byte(255 - i)
		}
		packet := &Packet{
			Header: Header{
				Version:        0,
				IsStartOfFrame: true,
				IsFinalOfFrame: false,
				Handle:         3333,
				SequenceNumber: 6666,
				FrameNumber:    9999,
				Timestamp:      0xdeadbeef,
			},
			Payload: payload,
		}
		if extParticipantSid, err := NewExtensionParticipantSid("test_participant"); err == nil {
			if ext, err := extParticipantSid.Marshal(); err == nil {
				packet.AddExtension(ext)
			}
		}
		rawPacket, err := packet.Marshal()
		require.NoError(t, err)

		expectedRawPacket := []byte{
			0x14, 0x00, 0x0d, 0x05, 0x1a, 0x0a, 0x27, 0x0f,
			0xde, 0xad, 0xbe, 0xef, 0x00, 0x04, 0x00, 0x01,
			0x00, 0x10, 0x74, 0x65, 0x73, 0x74, 0x5f, 0x70,
			0x61, 0x72, 0x74, 0x69, 0x63, 0x69, 0x70, 0x61,
			0x6e, 0x74, 0xff, 0xfe, 0xfd, 0xfc,
		}
		require.Equal(t, expectedRawPacket, rawPacket)

		var unmarshaled Packet
		err = unmarshaled.Unmarshal(rawPacket)
		require.NoError(t, err)
		require.Equal(t, packet, &unmarshaled)

		ext, err := unmarshaled.GetExtension(uint16(livekit.DataTrackExtensionID_DTEI_PARTICIPANT_SID))
		require.NoError(t, err)

		var extParticipantSid ExtensionParticipantSid
		require.NoError(t, extParticipantSid.Unmarshal(ext))
		require.Equal(t, livekit.ParticipantID("test_participant"), extParticipantSid.ParticipantID())
	})

	t.Run("with extension padding", func(t *testing.T) {
		payload := make([]byte, 4)
		for i := range len(payload) {
			payload[i] = byte(255 - i)
		}
		packet := &Packet{
			Header: Header{
				Version:        0,
				IsStartOfFrame: true,
				IsFinalOfFrame: false,
				Handle:         3333,
				SequenceNumber: 6666,
				FrameNumber:    9999,
				Timestamp:      0xdeadbeef,
			},
			Payload: payload,
		}
		if extParticipantSid, err := NewExtensionParticipantSid("participant"); err == nil {
			if ext, err := extParticipantSid.Marshal(); err == nil {
				packet.AddExtension(ext)
			}
		}
		rawPacket, err := packet.Marshal()
		require.NoError(t, err)

		expectedRawPacket := []byte{
			0x14, 0x00, 0x0d, 0x05, 0x1a, 0x0a, 0x27, 0x0f,
			0xde, 0xad, 0xbe, 0xef, 0x00, 0x03, 0x00, 0x01,
			0x00, 0x0b, 0x70, 0x61, 0x72, 0x74, 0x69, 0x63,
			0x69, 0x70, 0x61, 0x6e, 0x74, 0x00, 0xff, 0xfe,
			0xfd, 0xfc,
		}
		require.Equal(t, expectedRawPacket, rawPacket)

		var unmarshaled Packet
		err = unmarshaled.Unmarshal(rawPacket)
		require.NoError(t, err)
		require.Equal(t, packet, &unmarshaled)

		ext, err := unmarshaled.GetExtension(uint16(livekit.DataTrackExtensionID_DTEI_PARTICIPANT_SID))
		require.NoError(t, err)

		var extParticipantSid ExtensionParticipantSid
		require.NoError(t, extParticipantSid.Unmarshal(ext))
		require.Equal(t, livekit.ParticipantID("participant"), extParticipantSid.ParticipantID())
	})

	t.Run("replace extension", func(t *testing.T) {
		payload := make([]byte, 4)
		for i := range len(payload) {
			payload[i] = byte(255 - i)
		}
		packet := &Packet{
			Header: Header{
				Version:        0,
				IsStartOfFrame: true,
				IsFinalOfFrame: false,
				Handle:         3333,
				SequenceNumber: 6666,
				FrameNumber:    9999,
				Timestamp:      0xdeadbeef,
			},
			Payload: payload,
		}
		if extParticipantSid, err := NewExtensionParticipantSid("participant"); err == nil {
			if ext, err := extParticipantSid.Marshal(); err == nil {
				packet.AddExtension(ext)
			}
		}
		rawPacket, err := packet.Marshal()
		require.NoError(t, err)

		expectedRawPacket := []byte{
			0x14, 0x00, 0x0d, 0x05, 0x1a, 0x0a, 0x27, 0x0f,
			0xde, 0xad, 0xbe, 0xef, 0x00, 0x03, 0x00, 0x01,
			0x00, 0x0b, 0x70, 0x61, 0x72, 0x74, 0x69, 0x63,
			0x69, 0x70, 0x61, 0x6e, 0x74, 0x00, 0xff, 0xfe,
			0xfd, 0xfc,
		}
		require.Equal(t, expectedRawPacket, rawPacket)

		// replace existing extension ID and ensure that marshalled packet is updated
		if extParticipantSid, err := NewExtensionParticipantSid("test_participant"); err == nil {
			if ext, err := extParticipantSid.Marshal(); err == nil {
				packet.AddExtension(ext)
			}
		}
		rawPacket, err = packet.Marshal()
		require.NoError(t, err)

		expectedRawPacket = []byte{
			0x14, 0x00, 0x0d, 0x05, 0x1a, 0x0a, 0x27, 0x0f,
			0xde, 0xad, 0xbe, 0xef, 0x00, 0x04, 0x00, 0x01,
			0x00, 0x10, 0x74, 0x65, 0x73, 0x74, 0x5f, 0x70,
			0x61, 0x72, 0x74, 0x69, 0x63, 0x69, 0x70, 0x61,
			0x6e, 0x74, 0xff, 0xfe, 0xfd, 0xfc,
		}
		require.Equal(t, expectedRawPacket, rawPacket)

		var unmarshaled Packet
		err = unmarshaled.Unmarshal(rawPacket)
		require.NoError(t, err)
		require.Equal(t, packet, &unmarshaled)

		ext, err := unmarshaled.GetExtension(uint16(livekit.DataTrackExtensionID_DTEI_PARTICIPANT_SID))
		require.NoError(t, err)

		var extParticipantSid ExtensionParticipantSid
		require.NoError(t, extParticipantSid.Unmarshal(ext))
		require.Equal(t, livekit.ParticipantID("test_participant"), extParticipantSid.ParticipantID())
	})

	t.Run("bad pcaket", func(t *testing.T) {
		var unmarshaled Packet
		// extensions size too small
		badPacket := []byte{
			0x14, 0x00, 0x0d, 0x05, 0x1a, 0x0a, 0x27, 0x0f,
			0xde, 0xad, 0xbe, 0xef, 0x00, 0x02, 0x00, 0x01,
			0x00, 0x0b, 0x70, 0x61, 0x72, 0x74, 0x69, 0x63,
			0x69, 0x70, 0x61, 0x6e, 0x74, 0x00, 0xff, 0xfe,
			0xfd, 0xfc,
		}
		err := unmarshaled.Unmarshal(badPacket)
		require.Error(t, err)

		// get an invalid extension id
		badPacket = []byte{
			0x14, 0x00, 0x0d, 0x05, 0x1a, 0x0a, 0x27, 0x0f,
			0xde, 0xad, 0xbe, 0xef, 0x00, 0x03, 0x00, 0x02,
			0x00, 0x0b, 0x70, 0x61, 0x72, 0x74, 0x69, 0x63,
			0x69, 0x70, 0x61, 0x6e, 0x74, 0x00, 0xff, 0xfe,
			0xfd, 0xfc,
		}
		err = unmarshaled.Unmarshal(badPacket)
		require.NoError(t, err)
		_, err = unmarshaled.GetExtension(uint16(livekit.DataTrackExtensionID_DTEI_PARTICIPANT_SID))
		require.Error(t, err)

		// extension payload size bigger than payload
		badPacket = []byte{
			0x14, 0x00, 0x0d, 0x05, 0x1a, 0x0a, 0x27, 0x0f,
			0xde, 0xad, 0xbe, 0xef, 0x00, 0x03, 0x00, 0x01,
			0x00, 0x0d, 0x70, 0x61, 0x72, 0x74, 0x69, 0x63,
			0x69, 0x70, 0x61, 0x6e, 0x74, 0x00, 0xff, 0xfe,
			0xfd, 0xfc,
		}
		err = unmarshaled.Unmarshal(badPacket)
		require.Error(t, err)

		// extension payload size smaller than payload
		badPacket = []byte{
			0x14, 0x00, 0x0d, 0x05, 0x1a, 0x0a, 0x27, 0x0f,
			0xde, 0xad, 0xbe, 0xef, 0x00, 0x03, 0x00, 0x01,
			0x00, 0x07, 0x70, 0x61, 0x72, 0x74, 0x69, 0x63,
			0x69, 0x70, 0x61, 0x6e, 0x74, 0x00, 0xff, 0xfe,
			0xfd, 0xfc,
		}
		err = unmarshaled.Unmarshal(badPacket)
		require.Error(t, err)
	})
}
