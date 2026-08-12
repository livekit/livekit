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
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/rtc/types"
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

// dialCompressed opens a permessage-deflate signalling connection to /rtc and
// returns a valid, highly compressible SignalRequest whose marshaled (uncompressed)
// size exceeds sizeAtLeast. On the wire the frame compresses to a few hundred
// bytes, staying well under the transport read limit, so only the decompressed
// bound can reject it.
func dialCompressed(t *testing.T, identity string, sizeAtLeast int) (*websocket.Conn, []byte) {
	t.Helper()

	token := joinToken(testRoom, identity, nil)
	header := make(http.Header)
	testclient.SetAuthorizationToken(header, token)

	dialer := websocket.Dialer{EnableCompression: true}
	url := fmt.Sprintf(
		"ws://localhost:%d/rtc?protocol=%d&auto_subscribe=true&sdk=go",
		defaultServerPort, types.CurrentProtocol,
	)
	ws, _, err := dialer.Dial(url, header)
	require.NoError(t, err)
	ws.EnableWriteCompression(true)

	req := &livekit.SignalRequest{
		Message: &livekit.SignalRequest_UpdateMetadata{
			UpdateMetadata: &livekit.UpdateParticipantMetadata{
				Metadata: strings.Repeat("a", sizeAtLeast),
			},
		},
	}
	payload, err := proto.Marshal(req)
	require.NoError(t, err)
	require.Greater(t, len(payload), sizeAtLeast)

	return ws, payload
}

// TestSignalMessageSizeLimitDecompressed reproduces the permessage-deflate
// amplification case: a valid frame stays tiny on the wire (well under the
// transport read limit) but inflates past the limit once decompressed. The
// decompressed-size guard must reject it and the server must close the connection.
func TestSignalMessageSizeLimitDecompressed(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	const limit = 64 << 10 // 64 KiB
	_, finish := setupSingleNodeTestWithConfig("TestSignalMessageSizeLimitDecompressed", func(conf *config.Config) {
		conf.Limit.SignalMessageSizeLimit = limit
	})
	defer finish()

	ws, payload := dialCompressed(t, "compressed", limit*8)
	defer ws.Close()
	require.NoError(t, ws.WriteMessage(websocket.BinaryMessage, payload))

	// the server rejects the oversized decompressed message and tears down the
	// connection: draining eventually yields a close/EOF rather than a read timeout
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	var err error
	for {
		if _, _, err = ws.ReadMessage(); err != nil {
			break
		}
	}
	require.Error(t, err)
	require.False(t, isTimeout(err), "connection should be closed by the server, not time out; got %v", err)
	require.False(t, websocket.IsCloseError(err, websocket.CloseMessageTooBig), "got %v", err)
}

// TestSignalMessageSizeLimitDecompressedDisabled is the counterpart to
// TestSignalMessageSizeLimitDecompressed: with the limit disabled the same valid
// large frame is accepted and the connection stays open (the drain times out
// rather than being closed).
func TestSignalMessageSizeLimitDecompressedDisabled(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	_, finish := setupSingleNodeTestWithConfig("TestSignalMessageSizeLimitDecompressedDisabled", func(conf *config.Config) {
		conf.Limit.SignalMessageSizeLimit = 0
	})
	defer finish()

	ws, payload := dialCompressed(t, "compressed-disabled", 1<<20)
	defer ws.Close()
	require.NoError(t, ws.WriteMessage(websocket.BinaryMessage, payload))

	// the message is accepted; the connection stays open, so draining blocks until
	// the read deadline instead of observing a server-initiated close
	_ = ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	var err error
	for {
		if _, _, err = ws.ReadMessage(); err != nil {
			break
		}
	}
	require.Error(t, err)
	require.True(t, isTimeout(err), "connection should stay open and time out; got %v", err)
}

func isTimeout(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	return false
}
