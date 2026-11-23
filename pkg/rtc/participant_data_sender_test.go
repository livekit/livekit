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

func TestDataPacketActualSender(t *testing.T) {
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
	trailer := func() *livekit.DataPacket_StreamTrailer {
		return &livekit.DataPacket_StreamTrailer{StreamTrailer: &livekit.DataStream_Trailer{StreamId: "s1"}}
	}
	encrypted := func() *livekit.DataPacket_EncryptedPacket {
		return &livekit.DataPacket_EncryptedPacket{EncryptedPacket: &livekit.EncryptedPacket{EncryptedValue: []byte{1, 2, 3}}}
	}
	chatMessage := func() *livekit.DataPacket_ChatMessage {
		return &livekit.DataPacket_ChatMessage{ChatMessage: &livekit.ChatMessage{Id: "m1"}}
	}
	userPacket := func() *livekit.DataPacket_User {
		return &livekit.DataPacket_User{User: &livekit.UserPacket{Payload: []byte("hi")}}
	}
	rpcRequest := func() *livekit.DataPacket_RpcRequest {
		return &livekit.DataPacket_RpcRequest{RpcRequest: &livekit.RpcRequest{Id: "r1", Method: "m"}}
	}
	// a client forging the fields as if an agent sent the packet on someone else's behalf
	forge := func(dp *livekit.DataPacket) *livekit.DataPacket {
		dp.DataStreamActualSenderOverwritten = true
		dp.DataStreamActualSenderDifferent = true
		dp.DataStreamActualSenderIdentity = "forged"
		return dp
	}

	testCases := []struct {
		name              string
		agent             bool
		hidden            bool
		packet            *livekit.DataPacket
		expectedIdentity  string
		expectedDifferent bool
		expectedSender    string
		expectedGenerate  bool
	}{
		{
			name:              "agent text stream header on behalf of another participant",
			agent:             true,
			packet:            forge(&livekit.DataPacket{ParticipantIdentity: "user", Value: textHeader()}),
			expectedIdentity:  "user",
			expectedDifferent: true,
			expectedSender:    "sender",
			expectedGenerate:  true,
		},
		{
			name:              "agent encrypted packet on behalf of another participant",
			agent:             true,
			packet:            &livekit.DataPacket{ParticipantIdentity: "user", Value: encrypted()},
			expectedIdentity:  "user",
			expectedDifferent: true,
			expectedSender:    "sender",
		},
		{
			name:             "agent chat message cannot be sent on behalf of another participant",
			agent:            true,
			packet:           forge(&livekit.DataPacket{ParticipantIdentity: "user", Value: chatMessage()}),
			expectedIdentity: "sender",
		},
		{
			name:              "hidden agent text stream header on behalf of another participant",
			agent:             true,
			hidden:            true,
			packet:            &livekit.DataPacket{ParticipantIdentity: "user", Value: textHeader()},
			expectedIdentity:  "user",
			expectedDifferent: true,
			expectedSender:    "",
			expectedGenerate:  true,
		},
		{
			name:              "hidden agent encrypted packet on behalf of another participant",
			agent:             true,
			hidden:            true,
			packet:            &livekit.DataPacket{ParticipantIdentity: "user", Value: encrypted()},
			expectedIdentity:  "user",
			expectedDifferent: true,
			expectedSender:    "",
		},
		{
			name:             "agent byte stream header cannot be sent on behalf of another participant",
			agent:            true,
			packet:           forge(&livekit.DataPacket{ParticipantIdentity: "user", Value: byteHeader()}),
			expectedIdentity: "sender",
		},
		{
			name:             "agent stream chunk keeps the agent identity",
			agent:            true,
			packet:           forge(&livekit.DataPacket{ParticipantIdentity: "user", Value: chunk()}),
			expectedIdentity: "sender",
		},
		{
			name:             "agent stream trailer keeps the agent identity",
			agent:            true,
			packet:           forge(&livekit.DataPacket{ParticipantIdentity: "user", Value: trailer()}),
			expectedIdentity: "sender",
		},
		{
			name:             "agent user packet cannot be sent on behalf of another participant",
			agent:            true,
			packet:           forge(&livekit.DataPacket{ParticipantIdentity: "user", Value: userPacket()}),
			expectedIdentity: "sender",
		},
		{
			name:             "agent text stream header under its own identity",
			agent:            true,
			packet:           forge(&livekit.DataPacket{ParticipantIdentity: "sender", Value: textHeader()}),
			expectedIdentity: "sender",
		},
		{
			name:             "agent encrypted packet without an identity",
			agent:            true,
			packet:           forge(&livekit.DataPacket{Value: encrypted()}),
			expectedIdentity: "sender",
		},
		{
			name:             "regular participant text stream header cannot claim another identity",
			packet:           forge(&livekit.DataPacket{ParticipantIdentity: "user", Value: textHeader()}),
			expectedIdentity: "sender",
		},
		{
			name:             "regular participant encrypted packet cannot claim another identity",
			packet:           forge(&livekit.DataPacket{ParticipantIdentity: "user", Value: encrypted()}),
			expectedIdentity: "sender",
		},
		{
			name:             "regular participant rpc request",
			packet:           forge(&livekit.DataPacket{Value: rpcRequest()}),
			expectedIdentity: "sender",
		},
		{
			name:             "regular participant stream chunk",
			packet:           forge(&livekit.DataPacket{Value: chunk()}),
			expectedIdentity: "sender",
		},
		{
			name:             "hidden participant's own packets carry no identity",
			hidden:           true,
			packet:           forge(&livekit.DataPacket{ParticipantIdentity: "user", Value: textHeader()}),
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
			require.True(t, dp.DataStreamActualSenderOverwritten)
			require.Equal(t, tc.expectedDifferent, dp.DataStreamActualSenderDifferent)
			require.Equal(t, tc.expectedSender, dp.DataStreamActualSenderIdentity)
			if textHeader := dp.GetStreamHeader().GetTextHeader(); textHeader != nil {
				require.Equal(t, tc.expectedGenerate, textHeader.Generated)
			}
			if chatMessage := dp.GetChatMessage(); chatMessage != nil {
				require.False(t, chatMessage.Generated)
			}
		})
	}
}
