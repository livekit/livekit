package client

import (
	"container/ring"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
	"github.com/thoas/go-funk"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/proto/livekit"
)

type RTCClient struct {
	id                 string
	conn               *websocket.Conn
	PeerConn           *webrtc.PeerConnection
	localTracks        []webrtc.TrackLocal
	lock               sync.Mutex
	ctx                context.Context
	cancel             context.CancelFunc
	connected          bool
	iceConnected       bool
	paused             bool
	me                 *webrtc.MediaEngine // optional, populated only when receiving tracks
	subscribedTracks   map[string][]*webrtc.TrackRemote
	localParticipant   *livekit.ParticipantInfo
	remoteParticipants map[string]*livekit.ParticipantInfo

	// pending actions to start after connected to peer
	pendingCandidates   []*webrtc.ICECandidate
	pendingTrackWriters []*TrackWriter
	OnConnected         func()

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
	maxLogs        = 256
	extMimeMapping = map[string]string{
		".ivf":  webrtc.MimeTypeVP8,
		".h264": webrtc.MimeTypeH264,
		".ogg":  webrtc.MimeTypeOpus,
	}
)

func NewWebSocketConn(host, token string) (*websocket.Conn, error) {
	u, err := url.Parse(host + "/rtc")
	if err != nil {
		return nil, err
	}
	requestHeader := make(http.Header)
	SetAuthorizationToken(requestHeader, token)
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), requestHeader)
	return conn, err
}

func SetAuthorizationToken(header http.Header, token string) {
	header.Set("Authorization", "Bearer "+token)
}

func NewRTCClient(conn *websocket.Conn) (*RTCClient, error) {
	// Create a new RTCPeerConnection
	peerConn, err := webrtc.NewPeerConnection(rtcConf)
	if err != nil {
		return nil, err
	}

	logRing := ring.New(maxLogs)
	c := &RTCClient{
		conn:               conn,
		lock:               sync.Mutex{},
		pendingCandidates:  make([]*webrtc.ICECandidate, 0),
		localTracks:        make([]webrtc.TrackLocal, 0),
		subscribedTracks:   make(map[string][]*webrtc.TrackRemote),
		remoteParticipants: make(map[string]*livekit.ParticipantInfo),
		reader:             logRing,
		writer:             logRing,
		PeerConn:           peerConn,
		me:                 &webrtc.MediaEngine{},
	}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	c.me.RegisterDefaultCodecs()
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

	peerConn.OnTrack(func(track *webrtc.TrackRemote, rtpReceiver *webrtc.RTPReceiver) {
		c.AppendLog("track received", "label", track.StreamID(), "id", track.ID())
		go c.processTrack(track)
	})

	peerConn.OnNegotiationNeeded(func() {
		if !c.iceConnected {
			return
		}
		c.requestNegotiation()
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

			initialConnect := !c.iceConnected
			c.pendingTrackWriters = nil
			c.iceConnected = true

			if initialConnect && c.OnConnected != nil {
				go c.OnConnected()
			}
		}
	})

	return c, nil
}

func (c *RTCClient) ID() string {
	return c.id
}

// create an offer for the server
func (c *RTCClient) Run() error {
	go c.logLoop()

	c.conn.SetCloseHandler(func(code int, text string) error {
		// when closed, stop connection
		logger.Infow("connection closed", "code", code, "text", text)
		c.Stop()
		return nil
	})

	// create a data channel, in order to work
	dc, err := c.PeerConn.CreateDataChannel("_private", nil)
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
			c.id = msg.Join.Participant.Sid

			c.lock.Lock()
			for _, p := range msg.Join.OtherParticipants {
				c.remoteParticipants[p.Sid] = p
			}
			c.lock.Unlock()

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
			c.handleAnswer(rtc.FromProtoSessionDescription(msg.Answer))
		case *livekit.SignalResponse_Offer:
			c.AppendLog("received server offer",
				"type", msg.Offer.Type)
			desc := rtc.FromProtoSessionDescription(msg.Offer)
			if err := c.handleOffer(desc); err != nil {
				return err
			}
		case *livekit.SignalResponse_Negotiate:
			c.negotiate()
		case *livekit.SignalResponse_Trickle:
			candidateInit := rtc.FromProtoTrickle(msg.Trickle)
			c.AppendLog("adding remote candidate", "candidate", candidateInit.Candidate)
			if err := c.PeerConn.AddICECandidate(candidateInit); err != nil {
				return err
			}
		case *livekit.SignalResponse_Update:
			c.lock.Lock()
			for _, p := range msg.Update.Participants {
				c.remoteParticipants[p.Sid] = p
				c.AppendLog("participant update", "id", p.Sid, "state", p.State.String())
			}
			c.lock.Unlock()
		}
	}

	if err != io.EOF {
		return err
	}
	return nil
}

func (c *RTCClient) WaitUntilConnected() error {
	ctx, _ := context.WithTimeout(context.Background(), 5*time.Second)
	for {
		select {
		case <-ctx.Done():
			return errors.New("could not connect after timeout")
		case <-time.After(10 * time.Millisecond):
			if c.iceConnected {
				return nil
			}
		}
	}
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

func (c *RTCClient) SubscribedTracks() map[string][]*webrtc.TrackRemote {
	return c.subscribedTracks
}

func (c *RTCClient) RemoteParticipants() []*livekit.ParticipantInfo {
	return funk.Values(c.remoteParticipants).([]*livekit.ParticipantInfo)
}

func (c *RTCClient) Stop() {
	c.connected = false
	c.iceConnected = false
	c.conn.Close()
	c.PeerConn.Close()
	c.cancel()
}

func (c *RTCClient) PauseLogs() {
	c.paused = true
}

func (c *RTCClient) ResumeLogs() {
	c.paused = false
}

func (c *RTCClient) SendRequest(msg *livekit.SignalRequest) error {
	c.lock.Lock()
	defer c.lock.Unlock()
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

func (c *RTCClient) AddTrack(track *webrtc.TrackLocalStaticSample, path string) (writer *TrackWriter, err error) {
	trackType := livekit.TrackType_AUDIO
	if track.Kind() == webrtc.RTPCodecTypeVideo {
		trackType = livekit.TrackType_VIDEO
	}

	if err = c.SendAddTrack(track.ID(), track.StreamID(), trackType); err != nil {
		return
	}

	c.lock.Lock()
	defer c.lock.Unlock()
	c.localTracks = append(c.localTracks, track)

	if _, err = c.PeerConn.AddTrack(track); err != nil {
		return
	}

	writer = NewTrackWriter(c.ctx, track, path)

	// write tracks only after ICE connectivity
	if c.iceConnected {
		err = writer.Start()
	} else {
		c.pendingTrackWriters = append(c.pendingTrackWriters, writer)
	}

	return
}

func (c *RTCClient) AddStaticTrack(mime string, id string, label string) (writer *TrackWriter, err error) {
	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: mime}, id, label)
	if err != nil {
		return
	}

	return c.AddTrack(track, "")
}

func (c *RTCClient) AddFileTrack(path string, id string, label string) (writer *TrackWriter, err error) {
	// determine file mime
	mime, ok := extMimeMapping[filepath.Ext(path)]
	if !ok {
		return nil, fmt.Errorf("%s has an unsupported extension", filepath.Base(path))
	}

	c.AppendLog("adding track",
		"mime", mime,
	)

	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: mime},
		id,
		label,
	)
	if err != nil {
		return
	}

	return c.AddTrack(track, path)
}

// send AddTrack command to server to initiate server-side negotiation
func (c *RTCClient) SendAddTrack(cid string, name string, trackType livekit.TrackType) error {
	return c.SendRequest(&livekit.SignalRequest{
		Message: &livekit.SignalRequest_AddTrack{
			AddTrack: &livekit.AddTrackRequest{
				Cid:  cid,
				Name: name,
				Type: trackType,
			},
		},
	})
}

func (c *RTCClient) handleOffer(desc webrtc.SessionDescription) error {
	// always set remote description for both offer and answer
	if err := c.PeerConn.SetRemoteDescription(desc); err != nil {
		return err
	}

	// if we received an offer, we'd have to answer
	answer, err := c.PeerConn.CreateAnswer(nil)
	if err != nil {
		return err
	}

	if err := c.PeerConn.SetLocalDescription(answer); err != nil {
		return err
	}

	// send remote an answer
	c.AppendLog("sending answer")
	return c.SendRequest(&livekit.SignalRequest{
		Message: &livekit.SignalRequest_Answer{
			Answer: rtc.ToProtoSessionDescription(answer),
		},
	})
}

func (c *RTCClient) handleAnswer(desc webrtc.SessionDescription) error {
	c.AppendLog("handling server answer")
	// remote answered the offer, establish connection
	err := c.PeerConn.SetRemoteDescription(desc)
	if err != nil {
		return err
	}

	if !c.connected {
		c.connected = true

		// add all the pending items
		c.lock.Lock()
		for _, ic := range c.pendingCandidates {
			c.SendIceCandidate(ic)
		}
		c.pendingCandidates = nil
		c.lock.Unlock()
	}
	return nil
}

func (c *RTCClient) requestNegotiation() error {
	c.AppendLog("requesting negotiation")
	return c.SendRequest(&livekit.SignalRequest{
		Message: &livekit.SignalRequest_Negotiate{
			Negotiate: &livekit.NegotiationRequest{},
		},
	})
}

func (c *RTCClient) negotiate() error {
	c.AppendLog("starting negotiation")
	offer, err := c.PeerConn.CreateOffer(nil)
	if err != nil {
		return err
	}

	if err := c.PeerConn.SetLocalDescription(offer); err != nil {
		return err
	}

	return c.SendRequest(&livekit.SignalRequest{
		Message: &livekit.SignalRequest_Offer{
			Offer: rtc.ToProtoSessionDescription(offer),
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
				logger.Infow(val.msg, val.args...)
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

func (c *RTCClient) processTrack(track *webrtc.TrackRemote) {
	lastUpdate := time.Time{}
	pId, trackId := rtc.UnpackTrackId(track.ID())
	c.lock.Lock()
	c.subscribedTracks[pId] = append(c.subscribedTracks[pId], track)
	c.lock.Unlock()

	defer func() {
		c.lock.Lock()
		c.subscribedTracks[pId] = funk.Without(c.subscribedTracks[pId], track).([]*webrtc.TrackRemote)
		c.lock.Unlock()
	}()

	numBytes := 0
	for {
		pkt, _, err := track.ReadRTP()
		if c.ctx.Err() != nil {
			break
		}
		if rtc.IsEOF(err) {
			break
		}
		if err != nil {
			c.AppendLog("error reading RTP", "err", err)
			continue
		}
		numBytes += pkt.MarshalSize()
		if time.Now().Sub(lastUpdate) > 30*time.Second {
			c.AppendLog("consumed from participant",
				"track", trackId, "participant", pId,
				"size", numBytes)
			lastUpdate = time.Now()
		}
	}
}
