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
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// TestWSSignalConnectionIdleTimeout runs over a real WebSocket, since pongs are
// only processed inside gorilla's read path.
func TestWSSignalConnectionIdleTimeout(t *testing.T) {
	const idle = 200 * time.Millisecond

	// serve returns the server side of a connection with the idle timeout set,
	// and the client side for the test to drive
	serve := func(t *testing.T) (*service.WSSignalConnection, *websocket.Conn) {
		conns := make(chan *websocket.Conn, 1)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
			require.NoError(t, err)
			conns <- c
		}))
		t.Cleanup(srv.Close)

		client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = client.Close() })

		sc := service.NewWSSignalConnection(<-conns, 0)
		t.Cleanup(func() { _ = sc.Close() })
		require.NoError(t, sc.SetIdleTimeout(idle))
		return sc, client
	}

	// readErr reports when the server's pending read fails
	readErr := func(sc *service.WSSignalConnection) <-chan error {
		ch := make(chan error, 1)
		go func() {
			for {
				if _, _, err := sc.ReadWorkerMessage(); err != nil {
					ch <- err
					return
				}
			}
		}()
		return ch
	}

	requireTimeout := func(t *testing.T, err error) {
		var ne net.Error
		require.ErrorAs(t, err, &ne)
		require.True(t, ne.Timeout())
	}

	t.Run("silent peer times out", func(t *testing.T) {
		sc, _ := serve(t)
		select {
		case err := <-readErr(sc):
			requireTimeout(t, err)
		case <-time.After(5 * idle):
			t.Fatal("read did not time out")
		}
	})

	t.Run("pongs keep a quiet peer alive", func(t *testing.T) {
		sc, client := serve(t)
		errs := readErr(sc)

		// a worker with nothing to say still answers pings; unsolicited pongs
		// stand in for those answers without waiting on the 10s ping interval
		for range 12 { // 3 idle periods
			require.NoError(t, client.WriteControl(websocket.PongMessage, nil, time.Now().Add(time.Second)))
			select {
			case err := <-errs:
				t.Fatalf("read failed while peer was answering: %v", err)
			case <-time.After(idle / 4):
			}
		}

		select {
		case err := <-errs:
			requireTimeout(t, err)
		case <-time.After(5 * idle):
			t.Fatal("read did not time out after pongs stopped")
		}
	})

	t.Run("messages keep a peer alive", func(t *testing.T) {
		sc, client := serve(t)
		payload, err := proto.Marshal(&livekit.WorkerMessage{})
		require.NoError(t, err)

		for range 3 {
			time.Sleep(idle / 2)
			require.NoError(t, client.WriteMessage(websocket.BinaryMessage, payload))
			_, _, err := sc.ReadWorkerMessage()
			require.NoError(t, err)
		}
	})
}
