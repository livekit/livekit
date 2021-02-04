package client

import (
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
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	"github.com/thoas/go-funk"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/livekit-server/proto/livekit"
)

type RTCClient struct {
	id       string
	conn     *websocket.Conn
	PeerConn *webrtc.PeerConnection
	// sid => track
	localTracks        map[string]webrtc.TrackLocal
	lock               sync.Mutex
	wsLock             sync.Mutex
	ctx                context.Context
	cancel             context.CancelFunc
	connected          utils.AtomicFlag
	iceConnected       utils.AtomicFlag
	me                 *webrtc.MediaEngine // optional, populated only when receiving tracks
	subscribedTracks   map[string][]*webrtc.TrackRemote
	localParticipant   *livekit.ParticipantInfo
	remoteParticipants map[string]*livekit.ParticipantInfo

	// tracks waiting to be acked, cid => trackInfo
	pendingPublishedTracks map[string]*livekit.TrackInfo

	// pending actions to start after connected to peer
	pendingCandidates   []*webrtc.ICECandidate
	pendingTrackWriters []*TrackWriter
	OnConnected         func()

	// map of track Id and last packet
	lastPackets   map[string]*rtp.Packet
	bytesReceived map[string]uint64
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

	c := &RTCClient{
		conn:                   conn,
		pendingCandidates:      make([]*webrtc.ICECandidate, 0),
		localTracks:            make(map[string]webrtc.TrackLocal),
		pendingPublishedTracks: make(map[string]*livekit.TrackInfo),
		subscribedTracks:       make(map[string][]*webrtc.TrackRemote),
		remoteParticipants:     make(map[string]*livekit.ParticipantInfo),
		PeerConn:               peerConn,
		me:                     &webrtc.MediaEngine{},
		lastPackets:            make(map[string]*rtp.Packet),
		bytesReceived:          make(map[string]uint64),
	}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	c.me.RegisterDefaultCodecs()
	peerConn.OnICECandidate(func(ic *webrtc.ICECandidate) {
		if ic == nil {
			return
		}

		if !c.connected.Get() {
			c.lock.Lock()
			defer c.lock.Unlock()
			// not connected, save to pending
			c.pendingCandidates = append(c.pendingCandidates, ic)
			return
		}

		// send it through
		if err := c.SendIceCandidate(ic); err != nil {
			logger.Debugw("failed to send ice candidate", "err", err)
		}
	})

	peerConn.OnTrack(func(track *webrtc.TrackRemote, rtpReceiver *webrtc.RTPReceiver) {
		logger.Debugw("track received", "label", track.StreamID(), "id", track.ID())
		go c.processTrack(track)
	})

	peerConn.OnNegotiationNeeded(func() {
		if !c.iceConnected.Get() {
			return
		}
		c.requestNegotiation()
	})

	peerConn.OnDataChannel(func(channel *webrtc.DataChannel) {
	})

	peerConn.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		logger.Debugw("ICE state has changed", "state", connectionState.String(),
			"participant", c.localParticipant.Identity)
		if connectionState == webrtc.ICEConnectionStateConnected {
			// flush peers
			c.lock.Lock()
			defer c.lock.Unlock()
			for _, tw := range c.pendingTrackWriters {
				if err := tw.Start(); err != nil {
					logger.Debugw("track writer error", "err", err)
				}
			}

			initialConnect := !c.iceConnected.Get()
			c.pendingTrackWriters = nil
			c.iceConnected.TrySet(true)

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
	c.conn.SetCloseHandler(func(code int, text string) error {
		// when closed, stop connection
		logger.Infow("connection closed", "code", code, "text", text)
		c.Stop()
		return nil
	})

	// create a data channel, in order to work
	_, err := c.PeerConn.CreateDataChannel("_private", nil)
	if err != nil {
		return err
	}

	// run the session
	for {
		res, err := c.ReadResponse()
		if err != nil {
			return err
		}
		switch msg := res.Message.(type) {
		case *livekit.SignalResponse_Join:
			c.localParticipant = msg.Join.Participant
			c.id = msg.Join.Participant.Sid

			c.lock.Lock()
			for _, p := range msg.Join.OtherParticipants {
				c.remoteParticipants[p.Sid] = p
			}
			c.lock.Unlock()

			logger.Debugw("join accepted, sending offer..", "participant", msg.Join.Participant.Identity)
			logger.Debugw("other participants", "count", len(msg.Join.OtherParticipants))

			// Create an offer to send to the other process
			offer, err := c.PeerConn.CreateOffer(nil)
			if err != nil {
				return err
			}

			logger.Debugw("created offer", "participant", c.localParticipant.Identity)

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
			if err = c.SendRequest(req); err != nil {
				return err
			}

			defer c.PeerConn.Close()
		case *livekit.SignalResponse_Answer:
			c.handleAnswer(rtc.FromProtoSessionDescription(msg.Answer))
		case *livekit.SignalResponse_Offer:
			logger.Debugw("received server offer",
				"type", msg.Offer.Type)
			desc := rtc.FromProtoSessionDescription(msg.Offer)
			if err := c.handleOffer(desc); err != nil {
				return err
			}
		case *livekit.SignalResponse_Negotiate:
			c.negotiate()
		case *livekit.SignalResponse_Trickle:
			candidateInit := rtc.FromProtoTrickle(msg.Trickle)
			if err := c.PeerConn.AddICECandidate(candidateInit); err != nil {
				return err
			}
		case *livekit.SignalResponse_Update:
			c.lock.Lock()
			for _, p := range msg.Update.Participants {
				c.remoteParticipants[p.Sid] = p
			}
			c.lock.Unlock()
		case *livekit.SignalResponse_TrackPublished:
			logger.Debugw("track published", "track", msg.TrackPublished.Track.Name, "participant", c.localParticipant.Sid,
				"cid", msg.TrackPublished.Cid, "trackSid", msg.TrackPublished.Track.Sid)
			c.lock.Lock()
			c.pendingPublishedTracks[msg.TrackPublished.Cid] = msg.TrackPublished.Track
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
			id := c.ID()
			if c.localParticipant != nil {
				id = c.localParticipant.Identity
			}
			return fmt.Errorf("%s could not connect after timeout", id)
		case <-time.After(10 * time.Millisecond):
			if c.iceConnected.Get() {
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

// TODO: this function is not thread safe, need to cleanup
func (c *RTCClient) SubscribedTracks() map[string][]*webrtc.TrackRemote {
	return c.subscribedTracks
}

func (c *RTCClient) RemoteParticipants() []*livekit.ParticipantInfo {
	return funk.Values(c.remoteParticipants).([]*livekit.ParticipantInfo)
}

func (c *RTCClient) Stop() {
	c.connected.TrySet(false)
	c.iceConnected.TrySet(false)
	c.conn.Close()
	c.PeerConn.Close()
	c.cancel()
}

func (c *RTCClient) SendRequest(msg *livekit.SignalRequest) error {
	payload, err := protojson.Marshal(msg)
	if err != nil {
		return err
	}

	c.wsLock.Lock()
	defer c.wsLock.Unlock()
	return c.conn.WriteMessage(websocket.TextMessage, payload)
}

func (c *RTCClient) SendIceCandidate(ic *webrtc.ICECandidate) error {
	candInit := ic.ToJSON()
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

	// wait till track published message is received
	timeout := time.After(5 * time.Second)
	var ti *livekit.TrackInfo
	for {
		select {
		case <-timeout:
			return nil, errors.New("could not publish track after timeout")
		default:
			c.lock.Lock()
			ti = c.pendingPublishedTracks[track.ID()]
			c.lock.Unlock()
			if ti != nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if ti != nil {
			break
		}
	}

	c.lock.Lock()
	defer c.lock.Unlock()
	c.localTracks[ti.Sid] = track

	if _, err = c.PeerConn.AddTrack(track); err != nil {
		return
	}

	writer = NewTrackWriter(c.ctx, track, path)

	// write tracks only after ICE connectivity
	if c.iceConnected.Get() {
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

	logger.Debugw("adding track",
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
	logger.Debugw("sending answer")
	return c.SendRequest(&livekit.SignalRequest{
		Message: &livekit.SignalRequest_Answer{
			Answer: rtc.ToProtoSessionDescription(answer),
		},
	})
}

func (c *RTCClient) handleAnswer(desc webrtc.SessionDescription) error {
	logger.Debugw("handling server answer", "participant", c.localParticipant.Identity)
	// remote answered the offer, establish connection
	err := c.PeerConn.SetRemoteDescription(desc)
	if err != nil {
		return err
	}

	if !c.connected.TrySet(true) {
		// already connected
		return nil
	}

	// add all the pending items
	c.lock.Lock()
	for _, ic := range c.pendingCandidates {
		c.SendIceCandidate(ic)
	}
	c.pendingCandidates = nil
	c.lock.Unlock()
	return nil
}

func (c *RTCClient) requestNegotiation() error {
	logger.Debugw("requesting negotiation")
	return c.SendRequest(&livekit.SignalRequest{
		Message: &livekit.SignalRequest_Negotiate{
			Negotiate: &livekit.NegotiationRequest{},
		},
	})
}

func (c *RTCClient) negotiate() error {
	logger.Debugw("starting negotiation")
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
			logger.Debugw("error reading RTP", "err", err)
			continue
		}
		c.lock.Lock()
		c.lastPackets[pId] = pkt
		c.bytesReceived[pId] += uint64(pkt.MarshalSize())
		c.lock.Unlock()
		numBytes += pkt.MarshalSize()
		if time.Now().Sub(lastUpdate) > 30*time.Second {
			logger.Debugw("consumed from participant",
				"track", trackId, "participant", pId,
				"size", numBytes)
			lastUpdate = time.Now()
		}
	}
}

func (c *RTCClient) BytesReceived() uint64 {
	var total uint64
	c.lock.Lock()
	for _, size := range c.bytesReceived {
		total += size
	}
	c.lock.Unlock()
	return total
}

func (c *RTCClient) SendNacks(count int) {
	var packets []rtcp.Packet
	c.lock.Lock()
	for _, pkt := range c.lastPackets {
		seqs := make([]uint16, 0, count)
		for i := 0; i < count; i++ {
			seqs = append(seqs, pkt.SequenceNumber-uint16(i))
		}
		packets = append(packets, &rtcp.TransportLayerNack{
			MediaSSRC: pkt.SSRC,
			Nacks:     rtcp.NackPairsFromSequenceNumbers(seqs),
		})
	}
	c.lock.Unlock()

	c.PeerConn.WriteRTCP(packets)
}
