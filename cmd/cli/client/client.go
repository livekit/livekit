package client

import (
	"container/ring"
	"context"
	"io"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/livekit-server/proto/livekit"
)

type RTCClient struct {
	conn              *websocket.Conn
	PeerConn          *webrtc.PeerConnection
	pendingCandidates []*webrtc.ICECandidate
	lock              sync.Mutex
	ctx               context.Context
	cancel            context.CancelFunc
	connected         bool
	paused            bool

	// navigate log ring buffer. saving the last N entries
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
	maxLogs = 256
)

func NewRTCClient(conn *websocket.Conn) (*RTCClient, error) {
	// Create a new RTCPeerConnection
	peerConn, err := webrtc.NewPeerConnection(rtcConf)
	if err != nil {
		return nil, err
	}

	logRing := ring.New(maxLogs)
	c := &RTCClient{
		conn:              conn,
		lock:              sync.Mutex{},
		pendingCandidates: make([]*webrtc.ICECandidate, 0),
		reader:            logRing,
		writer:            logRing,
		PeerConn:          peerConn,
	}
	c.ctx, c.cancel = context.WithCancel(context.Background())

	// TODO: set up callbacks
	peerConn.OnICECandidate(func(ic *webrtc.ICECandidate) {
		if ic == nil {
			return
		}

		if !c.connected {
			c.lock.Lock()
			defer c.lock.Unlock()
			// not connected, save to pending
			c.pendingCandidates = append(c.pendingCandidates, ic)
			return
		}

		// send it through
		if err := c.SendIceCandidate(ic); err != nil {
			c.AppendLog("failed to send ice candidate", "err", err)
		}
	})

	peerConn.OnTrack(func(track *webrtc.Track, r *webrtc.RTPReceiver) {
		c.AppendLog("track received", "track", track.Label(), "ssrc", track.SSRC())
		// TODO: set up track consumer to read
	})

	peerConn.OnNegotiationNeeded(func() {
	})

	peerConn.OnDataChannel(func(channel *webrtc.DataChannel) {
	})

	return c, nil
}

// create an offer for the server
func (c *RTCClient) Run() error {
	go c.logLoop()

	c.conn.SetCloseHandler(func(code int, text string) error {
		// when closed, stop connection
		logger.GetLogger().Infow("connection closed", "code", code, "text", text)
		c.Stop()
		return nil
	})

	// create a data channel, in order to work
	dc, err := c.PeerConn.CreateDataChannel("default", nil)
	if err != nil {
		return err
	}
	dc.OnOpen(func() {
		c.AppendLog("data channel open")
	})

	c.AppendLog("creating offer")

	// Create an offer to send to the other process
	offer, err := c.PeerConn.CreateOffer(nil)
	if err != nil {
		return err
	}

	c.AppendLog("created offer", "offer", offer.SDP)

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
	c.AppendLog("connecting to remote...")
	if err = c.SendRequest(req); err != nil {
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
			c.AppendLog("connected to remote, setting desc")
			// remote answered the offer, establish connection
			err = c.PeerConn.SetRemoteDescription(service.FromProtoSessionDescription(msg.Answer))
			if err != nil {
				return err
			}
			c.connected = true

			// add all the pending items
			c.lock.Lock()
			for _, ic := range c.pendingCandidates {
				c.SendIceCandidate(ic)
			}
			c.pendingCandidates = nil
			c.lock.Unlock()
		case *livekit.SignalResponse_Negotiate:
		// TBD
		case *livekit.SignalResponse_Trickle:
			candidateInit := service.FromProtoTrickle(msg.Trickle)
			c.AppendLog("adding remote candidate", "candidate", candidateInit.Candidate)
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

		if c.ctx.Err() != nil {
			return nil, c.ctx.Err()
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

func (c *RTCClient) Stop() {
	c.cancel()
}

func (c *RTCClient) PauseLogs() {
	c.paused = true
}

func (c *RTCClient) ResumeLogs() {
	c.paused = false
}

func (c *RTCClient) SendRequest(msg *livekit.SignalRequest) error {
	payload, err := protojson.Marshal(msg)
	if err != nil {
		return err
	}

	return c.conn.WriteMessage(websocket.TextMessage, payload)
}

func (c *RTCClient) SendIceCandidate(ic *webrtc.ICECandidate) error {
	candInit := ic.ToJSON()
	c.AppendLog("sending trickle candidate", "candidate", candInit.Candidate)
	return c.SendRequest(&livekit.SignalRequest{
		Message: &livekit.SignalRequest_Trickle{
			Trickle: service.ToProtoTrickle(&candInit),
		},
	})
}

type logEntry struct {
	msg  string
	args []interface{}
}

func (c *RTCClient) AppendLog(msg string, args ...interface{}) {
	entry := &logEntry{
		msg:  msg,
		args: args,
	}

	// not locking this, log loss ok in multithreaded env
	writer := c.writer
	writer.Value = entry
	c.writer = writer.Next()
}

func (c *RTCClient) logLoop() {
	for {
		for !c.paused && c.reader != c.writer {
			val, _ := c.reader.Value.(*logEntry)
			if val != nil {
				logger.GetLogger().Infow(val.msg, val.args...)
			}
			// advance reader until writer
			c.reader = c.reader.Next()
		}

		// sleep or abort
		select {
		case <-c.ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}
