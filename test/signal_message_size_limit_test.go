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

package test

import (
	"fmt"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/config"
	testclient "github.com/livekit/livekit-server/test/client"
)

// TestSignalMessageSizeLimit drives the full /rtc signalling path: a real client
// connects to a single-node server configured with a small read-message size
// limit and sends an oversized frame. The server must reject it at the transport
// and close the connection with a 1009 (message too big) close code.
func TestSignalMessageSizeLimit(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	const limit = 1024
	_, finish := setupSingleNodeTestWithConfig("TestSignalMessageSizeLimit", func(conf *config.Config) {
		conf.Limit.SignalMessageSizeLimit = limit
	})
	defer finish()

	token := joinToken(testRoom, "oversized", nil)
	opts := &testclient.Options{AutoSubscribe: true}
	testRTCServicePathToTestClientOptions(testRTCServicePathv0, opts)

	ws, err := testclient.NewWebSocketConn(fmt.Sprintf("ws://localhost:%d", defaultServerPort), token, opts)
	require.NoError(t, err)
	defer ws.Close()

	// send a frame well over the configured limit
	require.NoError(t, ws.WriteMessage(websocket.BinaryMessage, make([]byte, limit*4)))

	// the server rejects the oversized frame and closes the connection; drain any
	// buffered server messages (e.g. JoinResponse) until the close surfaces
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		if _, _, err = ws.ReadMessage(); err != nil {
			break
		}
	}
	require.True(t, websocket.IsCloseError(err, websocket.CloseMessageTooBig), "got %v", err)
}

// TestSignalMessageSizeLimitDisabled verifies that a limit of 0 leaves the
// connection unbounded: a frame larger than the default limit is not rejected by
// the transport with a 1009 close, and the server still delivers its signalling
// messages (e.g. the JoinResponse).
func TestSignalMessageSizeLimitDisabled(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	_, finish := setupSingleNodeTestWithConfig("TestSignalMessageSizeLimitDisabled", func(conf *config.Config) {
		conf.Limit.SignalMessageSizeLimit = 0
	})
	defer finish()

	token := joinToken(testRoom, "oversized-disabled", nil)
	opts := &testclient.Options{AutoSubscribe: true}
	testRTCServicePathToTestClientOptions(testRTCServicePathv0, opts)

	ws, err := testclient.NewWebSocketConn(fmt.Sprintf("ws://localhost:%d", defaultServerPort), token, opts)
	require.NoError(t, err)
	defer ws.Close()

	// send a frame larger than the default 2 MiB limit; with the limit disabled the
	// transport must not reject it
	require.NoError(t, ws.WriteMessage(websocket.BinaryMessage, make([]byte, 3<<20)))

	// the server still delivers signalling (the JoinResponse) and never closes with
	// a 1009; drain until an error and assert it is not a message-too-big close
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	var reads int
	for {
		if _, _, err = ws.ReadMessage(); err != nil {
			break
		}
		reads++
	}
	require.Positive(t, reads, "expected to read at least one server message")
	require.False(t, websocket.IsCloseError(err, websocket.CloseMessageTooBig), "got %v", err)
}
