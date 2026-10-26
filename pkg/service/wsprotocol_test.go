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

package service_test

import (
	"bytes"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/rtc/types/typesfakes"
	"github.com/livekit/livekit-server/pkg/service"
)

// TestWSSignalConnectionMessageSizeLimit exercises the decompressed-size bound in
// WSSignalConnection. NextReader returns the (already decompressed) message
// stream, so a reader larger than the limit stands in for a small compressed
// frame that expands past the limit once inflated.
func TestWSSignalConnectionMessageSizeLimit(t *testing.T) {
	const limit = 1024

	t.Run("rejects message larger than limit", func(t *testing.T) {
		fake := &typesfakes.FakeWebsocketClient{}
		fake.NextReaderReturns(websocket.BinaryMessage, bytes.NewReader(make([]byte, limit+1)), nil)

		c := service.NewWSSignalConnection(fake, limit)
		_, _, err := c.ReadRequest()
		require.Error(t, err)
		require.Contains(t, err.Error(), "exceeds size limit")
	})

	t.Run("accepts message within limit", func(t *testing.T) {
		payload, err := proto.Marshal(&livekit.SignalRequest{})
		require.NoError(t, err)
		require.LessOrEqual(t, len(payload), limit)

		fake := &typesfakes.FakeWebsocketClient{}
		fake.NextReaderReturns(websocket.BinaryMessage, bytes.NewReader(payload), nil)

		c := service.NewWSSignalConnection(fake, limit)
		msg, _, err := c.ReadRequest()
		require.NoError(t, err)
		require.NotNil(t, msg)
	})

	t.Run("limit of zero reads unbounded payload", func(t *testing.T) {
		// a payload well beyond any typical limit is read in full when disabled
		payload, err := proto.Marshal(&livekit.SignalRequest{})
		require.NoError(t, err)

		fake := &typesfakes.FakeWebsocketClient{}
		fake.NextReaderReturns(websocket.BinaryMessage, bytes.NewReader(payload), nil)

		c := service.NewWSSignalConnection(fake, 0)
		_, _, err = c.ReadRequest()
		require.NoError(t, err)
	})
}
