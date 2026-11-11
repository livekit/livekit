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

package rtc

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/rtc/types/typesfakes"
)

func TestDataPacketSender(t *testing.T) {
	textHeader := func() *livekit.DataPacket_StreamHeader {
		return &livekit.DataPacket_StreamHeader{StreamHeader: &livekit.DataStream_Header{
			StreamId:      "s1",
			ContentHeader: &livekit.DataStream_Header_TextHeader{TextHeader: &livekit.DataStream_TextHeader{}},
		}}
	}
	byteHeader := func() *livekit.DataPacket_StreamHeader {
		return &livekit.DataPacket_StreamHeader{StreamHeader: &livekit.DataStream_Header{
			StreamId:      "s1",
			ContentHeader: &livekit.DataStream_Header_ByteHeader{ByteHeader: &livekit.DataStream_ByteHeader{}},
		}}
	}
	chunk := func() *livekit.DataPacket_StreamChunk {
		return &livekit.DataPacket_StreamChunk{StreamChunk: &livekit.DataStream_Chunk{StreamId: "s1"}}
	}
	chatMessage := func() *livekit.DataPacket_ChatMessage {
		return &livekit.DataPacket_ChatMessage{ChatMessage: &livekit.ChatMessage{Id: "m1"}}
	}
	forged := &livekit.DataPacket_SenderIdentity{SenderIdentity: "forged"}

	testCases := []struct {
		name             string
		agent            bool
		hidden           bool
		packet           *livekit.DataPacket
		expectedIdentity string
		expectedSender   any
	}{
		{
			name:             "regular participant cannot claim another identity or forge the sender",
			packet:           &livekit.DataPacket{ParticipantIdentity: "user", Sender: forged, Value: textHeader()},
			expectedIdentity: "sender",
		},
		{
			name:             "agent text stream header on behalf of another participant",
			agent:            true,
			packet:           &livekit.DataPacket{ParticipantIdentity: "user", Sender: forged, Value: textHeader()},
			expectedIdentity: "user",
			expectedSender:   &livekit.DataPacket_SenderIdentity{SenderIdentity: "sender"},
		},
		{
			name:             "hidden agent text stream header on behalf of another participant",
			agent:            true,
			hidden:           true,
			packet:           &livekit.DataPacket{ParticipantIdentity: "user", Value: textHeader()},
			expectedIdentity: "user",
			expectedSender:   &livekit.DataPacket_SenderHidden{SenderHidden: true},
		},
		{
			name:             "agent chat message on behalf of another participant",
			agent:            true,
			packet:           &livekit.DataPacket{ParticipantIdentity: "user", Value: chatMessage()},
			expectedIdentity: "user",
			expectedSender:   &livekit.DataPacket_SenderIdentity{SenderIdentity: "sender"},
		},
		{
			name:             "agent byte stream header cannot be sent on behalf of another participant",
			agent:            true,
			packet:           &livekit.DataPacket{ParticipantIdentity: "user", Sender: forged, Value: byteHeader()},
			expectedIdentity: "sender",
		},
		{
			name:             "agent stream chunk always carries the agent identity",
			agent:            true,
			packet:           &livekit.DataPacket{ParticipantIdentity: "user", Sender: forged, Value: chunk()},
			expectedIdentity: "sender",
		},
		{
			name:             "agent text stream header under its own identity",
			agent:            true,
			packet:           &livekit.DataPacket{ParticipantIdentity: "sender", Sender: forged, Value: textHeader()},
			expectedIdentity: "sender",
		},
		{
			name:             "agent text stream header without an identity",
			agent:            true,
			packet:           &livekit.DataPacket{Value: textHeader()},
			expectedIdentity: "sender",
		},
		{
			name:             "hidden participant's own packets carry neither identity nor sender",
			hidden:           true,
			packet:           &livekit.DataPacket{ParticipantIdentity: "user", Sender: forged, Value: textHeader()},
			expectedIdentity: "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			p := newParticipantForTest("sender")
			p.grants.Store(&auth.ClaimGrants{Video: &auth.VideoGrant{Agent: tc.agent, Hidden: tc.hidden}})

			p.handleReceivedDataMessage(livekit.DataPacket_RELIABLE, tc.packet)

			listener := p.listener().(*typesfakes.FakeLocalParticipantListener)
			require.Equal(t, 1, listener.OnDataMessageCallCount())
			_, _, dp := listener.OnDataMessageArgsForCall(0)
			require.Equal(t, tc.expectedIdentity, dp.ParticipantIdentity)
			require.Equal(t, tc.expectedSender, any(dp.GetSender()))
		})
	}
}
