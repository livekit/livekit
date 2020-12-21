package rtc

import (
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/proto/livekit"
)

const (
	pingFrequency = 10 * time.Second
	pingTimeout   = 2 * time.Second
)

type WSSignalConnection struct {
	conn    WebsocketClient
	mu      sync.Mutex
	useJSON bool
}

func NewWSSignalConnection(conn WebsocketClient) *WSSignalConnection {
	wsc := &WSSignalConnection{
		conn:    conn,
		mu:      sync.Mutex{},
		useJSON: true,
	}
	go wsc.pingWorker()
	return wsc
}

func (c *WSSignalConnection) ReadRequest() (*livekit.SignalRequest, error) {
	for {
		// handle special messages and pass on the rest
		messageType, payload, err := c.conn.ReadMessage()
		if err != nil {
			return nil, err
		}

		msg := &livekit.SignalRequest{}
		switch messageType {
		case websocket.BinaryMessage:
			// protobuf encoded
			err := proto.Unmarshal(payload, msg)
			return msg, err
		case websocket.TextMessage:
			// json encoded, also write back JSON
			c.useJSON = true
			err := protojson.Unmarshal(payload, msg)
			return msg, err
		default:
			logger.GetLogger().Debugw("unsupported message", "message", messageType)
			return nil, nil
		}
	}
}

func (c *WSSignalConnection) WriteResponse(msg *livekit.SignalResponse) error {
	var msgType int
	var payload []byte
	var err error

	if c.useJSON {
		msgType = websocket.TextMessage
		payload, err = protojson.Marshal(msg)
	} else {
		msgType = websocket.BinaryMessage
		payload, err = proto.Marshal(msg)
	}
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.WriteMessage(msgType, payload)
}

func (c *WSSignalConnection) pingWorker() {
	for {
		<-time.After(pingFrequency)
		err := c.conn.WriteControl(websocket.PingMessage, []byte(""), time.Now().Add(pingTimeout))
		if err != nil {
			return
		}
	}
}
