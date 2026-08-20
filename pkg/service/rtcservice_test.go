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

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/routing"
)

func leaveResponse(action livekit.LeaveRequest_Action) *livekit.SignalResponse {
	return &livekit.SignalResponse{
		Message: &livekit.SignalResponse_Leave{
			Leave: &livekit.LeaveRequest{
				Action: action,
				Reason: livekit.DisconnectReason_MIGRATION,
			},
		},
	}
}

func TestDrainMessageSource(t *testing.T) {
	collect := func(msgs *[]proto.Message) func(proto.Message) bool {
		return func(msg proto.Message) bool {
			*msgs = append(*msgs, msg)
			return true
		}
	}

	t.Run("drains queued messages of a closed source", func(t *testing.T) {
		source := routing.NewDefaultMessageChannel("CO_test")
		require.NoError(t, source.WriteMessage(leaveResponse(livekit.LeaveRequest_RESUME)))
		require.NoError(t, source.WriteMessage(leaveResponse(livekit.LeaveRequest_RECONNECT)))
		source.Close()

		var got []proto.Message
		require.True(t, drainMessageSource(source, time.Second, collect(&got)))
		require.Len(t, got, 2)
		require.Equal(
			t,
			livekit.LeaveRequest_RESUME,
			got[0].(*livekit.SignalResponse).GetLeave().GetAction(),
		)
		require.Equal(
			t,
			livekit.LeaveRequest_RECONNECT,
			got[1].(*livekit.SignalResponse).GetLeave().GetAction(),
		)
	})

	t.Run("drains messages written while draining", func(t *testing.T) {
		source := routing.NewDefaultMessageChannel("CO_test")
		// mimics the relay pushing a message that was still in flight when the
		// request direction went away, and closing the source right after
		go func() {
			time.Sleep(20 * time.Millisecond)
			_ = source.WriteMessage(leaveResponse(livekit.LeaveRequest_RESUME))
			source.Close()
		}()

		var got []proto.Message
		require.True(t, drainMessageSource(source, time.Second, collect(&got)))
		require.Len(t, got, 1)
	})

	t.Run("gives up on the deadline when the source stays open", func(t *testing.T) {
		source := routing.NewDefaultMessageChannel("CO_test")
		require.NoError(t, source.WriteMessage(leaveResponse(livekit.LeaveRequest_RESUME)))

		var got []proto.Message
		start := time.Now()
		require.False(t, drainMessageSource(source, 50*time.Millisecond, collect(&got)))
		require.GreaterOrEqual(t, time.Since(start), 50*time.Millisecond)
		// what was queued is still flushed
		require.Len(t, got, 1)
	})

	t.Run("stops when the write fails", func(t *testing.T) {
		source := routing.NewDefaultMessageChannel("CO_test")
		require.NoError(t, source.WriteMessage(leaveResponse(livekit.LeaveRequest_RESUME)))
		require.NoError(t, source.WriteMessage(leaveResponse(livekit.LeaveRequest_RECONNECT)))
		source.Close()

		var got []proto.Message
		require.False(t, drainMessageSource(source, time.Second, func(msg proto.Message) bool {
			got = append(got, msg)
			return false
		}))
		require.Len(t, got, 1)
	})
}
