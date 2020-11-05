package main

import (
	"container/ring"
	"io"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/livekit-server/proto/livekit"
)

type RTCClient struct {
	conn     *websocket.Conn
	PeerConn *webrtc.PeerConnection

	lock   sync.Mutex
	writer *ring.Ring
	reader *ring.Ring
}

var (
	// minimal settings only with stun server
	rtcConf = webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	}
)

func NewRTCClient(conn *websocket.Conn) (*RTCClient, error) {
	// Create a new RTCPeerConnection
	peerConn, err := webrtc.NewPeerConnection(rtcConf)
	if err != nil {
		return nil, err
	}

	c := &RTCClient{
		conn:     conn,
		lock:     sync.Mutex{},
		PeerConn: peerConn,
	}

	// TODO: set up callbacks

	return c, nil
}

// create an offer for the server
func (c *RTCClient) Run() error {
	// Create an offer to send to the other process
	offer, err := c.PeerConn.CreateOffer(nil)
	if err != nil {
		return err
	}

	// Sets the LocalDescription, and starts our UDP listeners
	// Note: this will start the gathering of ICE candidates
	if err = c.PeerConn.SetLocalDescription(offer); err != nil {
		return err
	}

	// send the offer to remote
	req := &livekit.SignalRequest{
		Message: &livekit.SignalRequest_Offer{
			Offer: service.ToProtoSessionDescription(offer),
		},
	}
	if err = c.WriteRequest(req); err != nil {
		return err
	}

	defer c.PeerConn.Close()

	// run the session
	for {
		res, err := c.ReadResponse()
		if err != nil {
			return err
		}
		switch msg := res.Message.(type) {
		case *livekit.SignalResponse_Answer:
			// remote answered the offer, establish connection
			err = c.PeerConn.SetRemoteDescription(service.FromProtoSessionDescription(msg.Answer))
			if err != nil {
				return err
			}
		case *livekit.SignalResponse_Negotiate:
		// TBD
		case *livekit.SignalResponse_Trickle:
			candidateInit := service.FromProtoTrickle(msg.Trickle)
			if err := c.PeerConn.AddICECandidate(*candidateInit); err != nil {
				return err
			}
		}
	}

	if err != io.EOF {
		return err
	}
	return nil
}

func (c *RTCClient) ReadResponse() (*livekit.SignalResponse, error) {
	for {
		// handle special messages and pass on the rest
		messageType, payload, err := c.conn.ReadMessage()
		if err != nil {
			return nil, err
		}

		msg := &livekit.SignalResponse{}
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
			err := protojson.Unmarshal(payload, msg)
			return msg, err
		default:
			return nil, nil
		}
	}
}

func (c *RTCClient) WriteRequest(msg *livekit.SignalRequest) error {
	payload, err := protojson.Marshal(msg)
	if err != nil {
		return err
	}

	return c.conn.WriteMessage(websocket.TextMessage, payload)
}
