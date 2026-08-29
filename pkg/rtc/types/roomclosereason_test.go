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

package types

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
)

// Pins the reason each close site used before RoomCloseReason was introduced, so the
// refactor cannot silently change what participants are told when a room closes.
func TestRoomCloseReasonToParticipantCloseReason(t *testing.T) {
	cases := []struct {
		reason   RoomCloseReason
		expected ParticipantCloseReason
	}{
		{RoomCloseReasonAPIDelete, ParticipantCloseReasonServiceRequestDeleteRoom},
		{RoomCloseReasonIdleTimeout, ParticipantCloseReasonRoomClosed},
		{RoomCloseReasonServerShutdown, ParticipantCloseReasonRoomManagerStop},
		{RoomCloseReasonSuperseded, ParticipantCloseReasonRoomClosed},
		{RoomCloseReasonOpenFailed, ParticipantCloseReasonNone},
		{RoomCloseReasonUnknown, ParticipantCloseReasonNone},
	}
	for _, c := range cases {
		t.Run(c.reason.String(), func(t *testing.T) {
			require.Equal(t, c.expected, c.reason.ToParticipantCloseReason())
		})
	}
}

func TestRoomCloseReasonToProto(t *testing.T) {
	cases := []struct {
		reason   RoomCloseReason
		expected livekit.RoomEndReason
	}{
		{RoomCloseReasonAPIDelete, livekit.RoomEndReason_ROOM_END_API_DELETE},
		{RoomCloseReasonIdleTimeout, livekit.RoomEndReason_ROOM_END_IDLE_TIMEOUT},
		{RoomCloseReasonServerShutdown, livekit.RoomEndReason_ROOM_END_SERVER_SHUTDOWN},
		{RoomCloseReasonSuperseded, livekit.RoomEndReason_ROOM_END_SUPERSEDED},
		{RoomCloseReasonOpenFailed, livekit.RoomEndReason_ROOM_END_OPEN_FAILED},
		{RoomCloseReasonUnknown, livekit.RoomEndReason_ROOM_END_UNKNOWN},
		{RoomCloseReason(99), livekit.RoomEndReason_ROOM_END_UNKNOWN},
	}
	for _, c := range cases {
		require.Equal(t, c.expected, c.reason.ToProto(), c.reason.String())
	}
}
