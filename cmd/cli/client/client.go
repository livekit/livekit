package client

import (
	"container/ring"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/randutil"
	"github.com/pion/webrtc/v3"
	"github.com/pkg/errors"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/proto/livekit"
)

type RTCClient struct {
	conn             *websocket.Conn
	PeerConn         *webrtc.PeerConnection
	localTracks      []*webrtc.Track
	lock             sync.Mutex
	ctx              context.Context
	cancel           context.CancelFunc
	connected        bool
	iceConnected     bool
	paused           bool
	me               *rtc.MediaEngine // optional, populated only when receiving tracks
	receivers        []*rtc.Receiver
	localParticipant *livekit.ParticipantInfo

	// pending actions to start after connected to peer
	pendingCandidates   []*webrtc.ICECandidate
	pendingTrackWriters []*TrackWriter

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
	maxLogs          = 256
	extFormatMapping = map[string]string{
		".ivf":  webrtc.VP8,
		".h264": webrtc.H264,
		".ogg":  webrtc.Opus,
	}
	payloadTypes = map[string]int{
		webrtc.VP8:  webrtc.DefaultPayloadTypeVP8,
		webrtc.H264: webrtc.DefaultPayloadTypeH264,
		webrtc.Opus: webrtc.DefaultPayloadTypeOpus,
	}
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
		localTracks:       make([]*webrtc.Track, 0),
		reader:            logRing,
		writer:            logRing,
		PeerConn:          peerConn,
	}
	c.ctx, c.cancel = context.WithCancel(context.Background())

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

	peerConn.OnTrack(func(track *webrtc.Track, rtpReceiver *webrtc.RTPReceiver) {
		c.AppendLog("track received", "label", track.Label(), "id", track.ID())
		peerId, _ := rtc.UnpackTrackId(track.ID())
		tccExt := 0
		if c.me != nil {
			tccExt = c.me.TCCExt
		}
		r := rtc.NewReceiver(c.ctx, peerId, rtpReceiver, rtc.ReceiverConfig{}, tccExt)
		c.lock.Lock()
		c.receivers = append(c.receivers, r)
		r.Start()
		go c.consumeReceiver(r)
		c.lock.Unlock()
	})

	peerConn.OnNegotiationNeeded(func() {
		c.AppendLog("negotiate needed")
		if !c.connected {
			c.AppendLog("not yet connected, skipping negotiate")
			return
		}
		err := c.Negotiate()
		if err != nil {
			c.AppendLog("error negotiating", "err", err)
		}
	})

	peerConn.OnDataChannel(func(channel *webrtc.DataChannel) {
	})

	peerConn.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		c.AppendLog("ICE state has changed", "state", connectionState.String())
		if connectionState == webrtc.ICEConnectionStateConnected {
			// flush peers
			c.lock.Lock()
			defer c.lock.Unlock()
			for _, tw := range c.pendingTrackWriters {
				if err := tw.Start(); err != nil {
					c.AppendLog("track writer error", "err", err)
				}
			}

			c.pendingTrackWriters = nil
			c.iceConnected = true
		}
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

	// run the session
	for {
		res, err := c.ReadResponse()
		if err != nil {
			return err
		}
		switch msg := res.Message.(type) {
		case *livekit.SignalResponse_Join:
			c.AppendLog("join accepted, sending offer..", "participant", msg.Join.Participant.Sid)
			c.localParticipant = msg.Join.Participant
			c.AppendLog("other participants", "count", len(msg.Join.OtherParticipants))

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
					Offer: rtc.ToProtoSessionDescription(offer),
				},
			}
			c.AppendLog("connecting to remote...")
			if err = c.SendRequest(req); err != nil {
				return err
			}

			defer c.PeerConn.Close()
		case *livekit.SignalResponse_Answer:
			c.AppendLog("connected to remote, setting desc")
			// remote answered the offer, establish connection
			err = c.PeerConn.SetRemoteDescription(rtc.FromProtoSessionDescription(msg.Answer))
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
			c.AppendLog("received negotiate",
				"type", msg.Negotiate.Type)
			desc := rtc.FromProtoSessionDescription(msg.Negotiate)
			if err := c.handleNegotiate(desc); err != nil {
				return err
			}
		case *livekit.SignalResponse_Trickle:
			candidateInit := rtc.FromProtoTrickle(msg.Trickle)
			c.AppendLog("adding remote candidate", "candidate", candidateInit.Candidate)
			if err := c.PeerConn.AddICECandidate(candidateInit); err != nil {
				return err
			}
		case *livekit.SignalResponse_Update:
			for _, p := range msg.Update.Participants {
				c.AppendLog("participant update", "id", p.Sid, "state", p.State.String())
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
	c.conn.Close()
	c.cancel()
}

func (c *RTCClient) PauseLogs() {
	c.paused = true
}

func (c *RTCClient) ResumeLogs() {
	c.paused = false
}

func (c *RTCClient) Receivers() []*rtc.Receiver {
	c.lock.Lock()
	defer c.lock.Unlock()
	return append([]*rtc.Receiver{}, c.receivers...)
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
			Trickle: rtc.ToProtoTrickle(candInit),
		},
	})
}

func (c *RTCClient) Negotiate() error {
	// Create an offer to send to the other process
	offer, err := c.PeerConn.CreateOffer(nil)
	if err != nil {
		return err
	}

	if err = c.PeerConn.SetLocalDescription(offer); err != nil {
		return err
	}

	// send the offer to remote
	req := &livekit.SignalRequest{
		Message: &livekit.SignalRequest_Negotiate{
			Negotiate: rtc.ToProtoSessionDescription(offer),
		},
	}
	c.AppendLog("sending negotiate offer to remote...")
	if err = c.SendRequest(req); err != nil {
		return err
	}
	return nil
}

func (c *RTCClient) handleNegotiate(desc webrtc.SessionDescription) error {
	// always set remote description for both offer and answer
	if err := c.PeerConn.SetRemoteDescription(desc); err != nil {
		return err
	}

	// if we received an offer, we'd have to answer
	if desc.Type == webrtc.SDPTypeOffer {
		// create media engine
		c.me = &rtc.MediaEngine{}
		if err := c.me.PopulateFromSDP(desc); err != nil {
			return errors.Wrapf(err, "could not parse SDP")
		}

		answer, err := c.PeerConn.CreateAnswer(nil)
		if err != nil {
			return err
		}

		if err := c.PeerConn.SetLocalDescription(answer); err != nil {
			return err
		}

		// send remote an answer
		return c.SendRequest(&livekit.SignalRequest{
			Message: &livekit.SignalRequest_Negotiate{
				Negotiate: rtc.ToProtoSessionDescription(answer),
			},
		})
	}
	return nil
}

func (c *RTCClient) AddTrack(path string, codecType webrtc.RTPCodecType, id string, label string) error {
	// determine file type
	format, ok := extFormatMapping[filepath.Ext(path)]
	if !ok {
		return fmt.Errorf("%s has an unsupported extension", filepath.Base(path))
	}
	payloadType := uint8(payloadTypes[format])

	logger.GetLogger().Infow("adding track",
		"format", format,
	)
	track, err := c.PeerConn.NewTrack(payloadType, randutil.NewMathRandomGenerator().Uint32(), id, label)
	if err != nil {
		return err
	}

	c.lock.Lock()
	defer c.lock.Unlock()
	c.localTracks = append(c.localTracks, track)

	if _, err := c.PeerConn.AddTrack(track); err != nil {
		return err
	}

	tw := NewTrackWriter(c.ctx, track, path, format)

	// write tracks only after ICE connectivity
	if c.iceConnected {
		return tw.Start()
	} else {
		c.pendingTrackWriters = append(c.pendingTrackWriters, tw)
		return nil
	}
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

func (c *RTCClient) consumeReceiver(r *rtc.Receiver) {
	lastUpdate := time.Time{}
	peerId, trackId := rtc.UnpackTrackId(r.TrackId())
	numBytes := 0
	for {
		select {
		case packet, ok := <-r.RTPChan():
			if !ok {
				// channel closed, we are done
				return
			}
			numBytes += packet.MarshalSize()
			if time.Now().Sub(lastUpdate) > 30*time.Second {
				c.AppendLog("consumed from peer",
					"track", trackId, "peer", peerId,
					"size", numBytes)
				lastUpdate = time.Now()
			}
		case <-c.ctx.Done():
			return
		}

		if c.ctx.Err() != nil {
			return
		}
	}
}
