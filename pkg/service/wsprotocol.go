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

package service

import (
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/rtc/types"
)

const (
	pingFrequency = 10 * time.Second
	pingTimeout   = 2 * time.Second
)

type WSSignalConnection struct {
	conn    types.WebsocketClient
	mu      sync.Mutex
	useJSON bool
}

func NewWSSignalConnection(conn types.WebsocketClient) *WSSignalConnection {
	wsc := &WSSignalConnection{
		conn:    conn,
		mu:      sync.Mutex{},
		useJSON: false,
	}
	go wsc.pingWorker()
	return wsc
}

func (c *WSSignalConnection) Close() error {
	return c.conn.Close()
}

func (c *WSSignalConnection) SetReadDeadline(deadline time.Time) error {
	return c.conn.SetReadDeadline(deadline)
}

func (c *WSSignalConnection) ReadRequest() (*livekit.SignalRequest, int, error) {
	// handle special messages and pass on the rest
	messageType, payload, err := c.conn.ReadMessage()
	if err != nil {
		return nil, 0, err
	}

	msg := &livekit.SignalRequest{}
	switch messageType {
	case websocket.BinaryMessage:
		if c.useJSON {
			c.mu.Lock()
			// switch to protobuf if client supports it
			c.useJSON = false
			c.mu.Unlock()
		}
		// protobuf encoded
		err := proto.Unmarshal(payload, msg)
		return msg, len(payload), err
	case websocket.TextMessage:
		c.mu.Lock()
		// json encoded, also write back JSON
		c.useJSON = true
		c.mu.Unlock()
		err := protojson.Unmarshal(payload, msg)
		return msg, len(payload), err
	default:
		logger.Debugw("unsupported message", "message", messageType)
		return nil, len(payload), nil
	}
}

func (c *WSSignalConnection) ReadWorkerMessage() (*livekit.WorkerMessage, int, error) {
	// handle special messages and pass on the rest
	messageType, payload, err := c.conn.ReadMessage()
	if err != nil {
		return nil, 0, err
	}

	msg := &livekit.WorkerMessage{}
	switch messageType {
	case websocket.BinaryMessage:
		if c.useJSON {
			c.mu.Lock()
			// switch to protobuf if client supports it
			c.useJSON = false
			c.mu.Unlock()
		}
		// protobuf encoded
		err := proto.Unmarshal(payload, msg)
		return msg, len(payload), err
	case websocket.TextMessage:
		c.mu.Lock()
		// json encoded, also write back JSON
		c.useJSON = true
		c.mu.Unlock()
		err := protojson.Unmarshal(payload, msg)
		return msg, len(payload), err
	default:
		logger.Debugw("unsupported message", "message", messageType)
		return nil, len(payload), nil
	}
}

func (c *WSSignalConnection) WriteResponse(msg *livekit.SignalResponse) (int, error) {
	var msgType int
	var payload []byte
	var err error

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.useJSON {
		msgType = websocket.TextMessage
		payload, err = protojson.Marshal(msg)
	} else {
		msgType = websocket.BinaryMessage
		payload, err = proto.Marshal(msg)
	}
	if err != nil {
		return 0, err
	}

	return len(payload), c.conn.WriteMessage(msgType, payload)
}

func (c *WSSignalConnection) WriteServerMessage(msg *livekit.ServerMessage) (int, error) {
	var msgType int
	var payload []byte
	var err error

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.useJSON {
		msgType = websocket.TextMessage
		payload, err = protojson.Marshal(msg)
	} else {
		msgType = websocket.BinaryMessage
		payload, err = proto.Marshal(msg)
	}
	if err != nil {
		return 0, err
	}

	return len(payload), c.conn.WriteMessage(msgType, payload)
}

func (c *WSSignalConnection) pingWorker() {
	ticker := time.NewTicker(pingFrequency)
	defer ticker.Stop()

	for range ticker.C {
		err := c.conn.WriteControl(websocket.PingMessage, []byte(""), time.Now().Add(pingTimeout))
		if err != nil {
			return
		}
	}
}

// IsWebSocketCloseError checks that error is normal/expected closure
func IsWebSocketCloseError(err error) bool {
	return errors.Is(err, io.EOF) ||
		strings.HasSuffix(err.Error(), "use of closed network connection") ||
		strings.HasSuffix(err.Error(), "connection reset by peer") ||
		websocket.IsCloseError(
			err,
			websocket.CloseAbnormalClosure,
			websocket.CloseGoingAway,
			websocket.CloseNormalClosure,
			websocket.CloseNoStatusReceived,
		)
}
