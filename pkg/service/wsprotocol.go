package service

import (
	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/proto/livekit"
)

type SignalConnection interface {
	PeerId() string
	ReadRequest() (*livekit.SignalRequest, error)
	WriteResponse(*livekit.SignalResponse) error
}

type WSSignalConnection struct {
	peerId  string
	conn    *websocket.Conn
	useJSON bool
}

func NewWSSignalConnection(conn *websocket.Conn, peerId string) *WSSignalConnection {
	return &WSSignalConnection{
		peerId: peerId,
		conn:   conn,
	}
}

func (c *WSSignalConnection) PeerId() string {
	return c.peerId
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
		case websocket.PingMessage:
			c.conn.WriteMessage(websocket.PongMessage, nil)
			continue
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
			return nil, nil
		}
	}
}

func (c *WSSignalConnection) WriteResponse(msg *livekit.SignalResponse) error {
	var msgType int
	var payload []byte
	var err error

	if c.useJSON {
		payload, err = protojson.Marshal(msg)
	} else {
		payload, err = proto.Marshal(msg)
	}
	if err != nil {
		return err
	}

	return c.conn.WriteMessage(msgType, payload)
}
