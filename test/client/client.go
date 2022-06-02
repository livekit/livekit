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
	"go.uber.org/atomic"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/rtc"
)

const (
	lossyDataChannel    = "_lossy"
	reliableDataChannel = "_reliable"
)

type RTCClient struct {
	id         livekit.ParticipantID
	conn       *websocket.Conn
	publisher  *rtc.PCTransport
	subscriber *rtc.PCTransport
	// sid => track
	localTracks        map[string]webrtc.TrackLocal
	trackSenders       map[string]*webrtc.RTPSender
	lock               sync.Mutex
	wsLock             sync.Mutex
	ctx                context.Context
	cancel             context.CancelFunc
	connected          atomic.Bool
	iceConnected       atomic.Bool
	me                 *webrtc.MediaEngine // optional, populated only when receiving tracks
	subscribedTracks   map[livekit.ParticipantID][]*webrtc.TrackRemote
	localParticipant   *livekit.ParticipantInfo
	remoteParticipants map[livekit.ParticipantID]*livekit.ParticipantInfo

	reliableDC          *webrtc.DataChannel
	reliableDCSub       *webrtc.DataChannel
	lossyDC             *webrtc.DataChannel
	lossyDCSub          *webrtc.DataChannel
	publisherConnected  atomic.Bool
	publisherNegotiated atomic.Bool

	// tracks waiting to be acked, cid => trackInfo
	pendingPublishedTracks map[string]*livekit.TrackInfo

	pendingTrackWriters []*TrackWriter
	OnConnected         func()
	OnDataReceived      func(data []byte, sid string)
	refreshToken        string

	// map of livekit.ParticipantID and last packet
	lastPackets   map[livekit.ParticipantID]*rtp.Packet
	bytesReceived map[livekit.ParticipantID]uint64
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

type Options struct {
	AutoSubscribe bool
	Publish       string
}

func NewWebSocketConn(host, token string, opts *Options) (*websocket.Conn, error) {
	u, err := url.Parse(host + "/rtc?protocol=7")
	if err != nil {
		return nil, err
	}
	requestHeader := make(http.Header)
	SetAuthorizationToken(requestHeader, token)

	connectUrl := u.String()
	if opts != nil {
		connectUrl = fmt.Sprintf("%s&auto_subscribe=%t&publish=%s",
			connectUrl, opts.AutoSubscribe, opts.Publish)
	}
	conn, _, err := websocket.DefaultDialer.Dial(connectUrl, requestHeader)
	return conn, err
}

func SetAuthorizationToken(header http.Header, token string) {
	header.Set("Authorization", "Bearer "+token)
}

func NewRTCClient(conn *websocket.Conn) (*RTCClient, error) {
	var err error

	c := &RTCClient{
		conn:                   conn,
		localTracks:            make(map[string]webrtc.TrackLocal),
		trackSenders:           make(map[string]*webrtc.RTPSender),
		pendingPublishedTracks: make(map[string]*livekit.TrackInfo),
		subscribedTracks:       make(map[livekit.ParticipantID][]*webrtc.TrackRemote),
		remoteParticipants:     make(map[livekit.ParticipantID]*livekit.ParticipantInfo),
		me:                     &webrtc.MediaEngine{},
		lastPackets:            make(map[livekit.ParticipantID]*rtp.Packet),
		bytesReceived:          make(map[livekit.ParticipantID]uint64),
	}
	c.ctx, c.cancel = context.WithCancel(context.Background())

	conf := rtc.WebRTCConfig{
		Configuration: rtcConf,
	}
	conf.SettingEngine.SetLite(false)
	conf.SettingEngine.SetAnsweringDTLSRole(webrtc.DTLSRoleClient)
	codecs := []*livekit.Codec{
		{
			Mime: "audio/opus",
		},
		{
			Mime: "video/vp8",
		},
		{
			Mime: "video/h264",
		},
	}
	c.publisher, err = rtc.NewPCTransport(rtc.TransportParams{
		Target:        livekit.SignalTarget_PUBLISHER,
		Config:        &conf,
		EnabledCodecs: codecs,
	})
	if err != nil {
		return nil, err
	}
	// intentionally use publisher transport to have codecs pre-registered
	c.subscriber, err = rtc.NewPCTransport(rtc.TransportParams{
		Target:        livekit.SignalTarget_PUBLISHER,
		Config:        &conf,
		EnabledCodecs: codecs,
	})
	if err != nil {
		return nil, err
	}

	ordered := true
	c.reliableDC, err = c.publisher.PeerConnection().CreateDataChannel(reliableDataChannel,
		&webrtc.DataChannelInit{Ordered: &ordered},
	)
	if err != nil {
		return nil, err
	}

	maxRetransmits := uint16(0)
	c.lossyDC, err = c.publisher.PeerConnection().CreateDataChannel(lossyDataChannel,
		&webrtc.DataChannelInit{Ordered: &ordered, MaxRetransmits: &maxRetransmits},
	)
	if err != nil {
		return nil, err
	}

	c.publisher.PeerConnection().OnICECandidate(func(ic *webrtc.ICECandidate) {
		if ic == nil {
			return
		}
		_ = c.SendIceCandidate(ic, livekit.SignalTarget_PUBLISHER)
	})
	c.subscriber.PeerConnection().OnICECandidate(func(ic *webrtc.ICECandidate) {
		if ic == nil {
			return
		}
		_ = c.SendIceCandidate(ic, livekit.SignalTarget_SUBSCRIBER)
	})

	c.subscriber.PeerConnection().OnTrack(func(track *webrtc.TrackRemote, rtpReceiver *webrtc.RTPReceiver) {
		go c.processTrack(track)
	})
	c.subscriber.PeerConnection().OnDataChannel(func(channel *webrtc.DataChannel) {
		if channel.Label() == reliableDataChannel {
			c.reliableDCSub = channel
		} else if channel.Label() == lossyDataChannel {
			c.lossyDCSub = channel
		} else {
			return
		}
		channel.OnMessage(c.handleDataMessage)
	})

	c.publisher.OnOffer(c.onOffer)

	c.subscriber.PeerConnection().OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		logger.Debugw("subscriber ICE state has changed", "state", connectionState.String(),
			"participant", c.localParticipant.Identity)
		if connectionState == webrtc.ICEConnectionStateConnected {
			// flush peers
			c.lock.Lock()
			defer c.lock.Unlock()
			for _, tw := range c.pendingTrackWriters {
				if err := tw.Start(); err != nil {
					logger.Errorw("track writer error", err)
				}
			}

			initialConnect := !c.iceConnected.Load()
			c.pendingTrackWriters = nil
			c.iceConnected.Store(true)

			if initialConnect && c.OnConnected != nil {
				go c.OnConnected()
			}
		}
	})

	c.publisher.PeerConnection().OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		logger.Infow("publisher ICE state changed", "state", state.String(),
			"participant", c.localParticipant.Identity)

		if state == webrtc.ICEConnectionStateConnected {
			c.publisherConnected.Store(true)
			// check if publisher triggered negotiate (!subscriberPrimary)
			if c.publisherNegotiated.Load() {
				c.iceConnected.Store(true)
			}
		} else {
			c.publisherConnected.Store(false)
		}
	})

	return c, nil
}

func (c *RTCClient) ID() livekit.ParticipantID {
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

	// run the session
	for {
		res, err := c.ReadResponse()
		if errors.Is(io.EOF, err) {
			return nil
		} else if err != nil {
			logger.Errorw("error while reading", err)
			return err
		}
		switch msg := res.Message.(type) {
		case *livekit.SignalResponse_Join:
			c.localParticipant = msg.Join.Participant
			c.id = livekit.ParticipantID(msg.Join.Participant.Sid)
			c.lock.Lock()
			for _, p := range msg.Join.OtherParticipants {
				c.remoteParticipants[livekit.ParticipantID(p.Sid)] = p
			}
			c.lock.Unlock()
			// if publish only, negotiate
			if !msg.Join.SubscriberPrimary {
				c.publisherNegotiated.Store(true)
				c.publisher.Negotiate(false)
			}

			logger.Infow("join accepted, awaiting offer", "participant", msg.Join.Participant.Identity)
		case *livekit.SignalResponse_Answer:
			// logger.Debugw("received server answer",
			//	"participant", c.localParticipant.Identity,
			//	"answer", msg.Answer.Sdp)
			_ = c.handleAnswer(rtc.FromProtoSessionDescription(msg.Answer))
		case *livekit.SignalResponse_Offer:
			logger.Infow("received server offer",
				"participant", c.localParticipant.Identity,
			)
			desc := rtc.FromProtoSessionDescription(msg.Offer)
			if err := c.handleOffer(desc); err != nil {
				return err
			}
		case *livekit.SignalResponse_Trickle:
			candidateInit, err := rtc.FromProtoTrickle(msg.Trickle)
			if err != nil {
				return err
			}
			if msg.Trickle.Target == livekit.SignalTarget_PUBLISHER {
				err = c.publisher.AddICECandidate(candidateInit)
			} else {
				err = c.subscriber.AddICECandidate(candidateInit)
			}
			if err != nil {
				return err
			}
		case *livekit.SignalResponse_Update:
			c.lock.Lock()
			for _, p := range msg.Update.Participants {
				if livekit.ParticipantID(p.Sid) != c.id {
					if p.State != livekit.ParticipantInfo_DISCONNECTED {
						c.remoteParticipants[livekit.ParticipantID(p.Sid)] = p
					} else {
						delete(c.remoteParticipants, livekit.ParticipantID(p.Sid))
					}
				}
			}
			c.lock.Unlock()

		case *livekit.SignalResponse_TrackPublished:
			logger.Debugw("track published", "trackID", msg.TrackPublished.Track.Name, "participant", c.localParticipant.Sid,
				"cid", msg.TrackPublished.Cid, "trackSid", msg.TrackPublished.Track.Sid)
			c.lock.Lock()
			c.pendingPublishedTracks[msg.TrackPublished.Cid] = msg.TrackPublished.Track
			c.lock.Unlock()
		case *livekit.SignalResponse_RefreshToken:
			c.lock.Lock()
			c.refreshToken = msg.RefreshToken
			c.lock.Unlock()
		case *livekit.SignalResponse_TrackUnpublished:
			sid := msg.TrackUnpublished.TrackSid
			c.lock.Lock()
			sender := c.trackSenders[sid]
			if sender != nil {
				if err := c.publisher.PeerConnection().RemoveTrack(sender); err != nil {
					logger.Errorw("Could not unpublish track", err)
				}
				c.publisher.Negotiate(false)
			}
			delete(c.trackSenders, sid)
			delete(c.localTracks, sid)
			c.lock.Unlock()
		}
	}
}

func (c *RTCClient) WaitUntilConnected() error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			id := string(c.ID())
			if c.localParticipant != nil {
				id = c.localParticipant.Identity
			}
			return fmt.Errorf("%s could not connect after timeout", id)
		case <-time.After(10 * time.Millisecond):
			if c.iceConnected.Load() {
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
			_ = c.conn.WriteMessage(websocket.PongMessage, nil)
			continue
		case websocket.BinaryMessage:
			// protobuf encoded
			err := proto.Unmarshal(payload, msg)
			return msg, err
		default:
			return nil, fmt.Errorf("unexpected message received: %v", messageType)
		}
	}
}

func (c *RTCClient) SubscribedTracks() map[livekit.ParticipantID][]*webrtc.TrackRemote {
	// create a copy of this
	c.lock.Lock()
	defer c.lock.Unlock()
	tracks := make(map[livekit.ParticipantID][]*webrtc.TrackRemote, len(c.subscribedTracks))
	for key, val := range c.subscribedTracks {
		tracks[key] = val
	}
	return tracks
}

func (c *RTCClient) RemoteParticipants() []*livekit.ParticipantInfo {
	c.lock.Lock()
	defer c.lock.Unlock()
	return funk.Values(c.remoteParticipants).([]*livekit.ParticipantInfo)
}

func (c *RTCClient) GetRemoteParticipant(sid livekit.ParticipantID) *livekit.ParticipantInfo {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.remoteParticipants[sid]
}

func (c *RTCClient) Stop() {
	logger.Infow("stopping client", "ID", c.ID())
	_ = c.SendRequest(&livekit.SignalRequest{
		Message: &livekit.SignalRequest_Leave{
			Leave: &livekit.LeaveRequest{},
		},
	})
	c.connected.Store(false)
	c.iceConnected.Store(false)
	_ = c.conn.Close()
	c.publisher.Close()
	c.subscriber.Close()
	c.cancel()
}

func (c *RTCClient) RefreshToken() string {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.refreshToken
}

func (c *RTCClient) SendRequest(msg *livekit.SignalRequest) error {
	payload, err := proto.Marshal(msg)
	if err != nil {
		return err
	}

	c.wsLock.Lock()
	defer c.wsLock.Unlock()
	return c.conn.WriteMessage(websocket.BinaryMessage, payload)
}

func (c *RTCClient) SendIceCandidate(ic *webrtc.ICECandidate, target livekit.SignalTarget) error {
	trickle := rtc.ToProtoTrickle(ic.ToJSON())
	trickle.Target = target
	return c.SendRequest(&livekit.SignalRequest{
		Message: &livekit.SignalRequest_Trickle{
			Trickle: trickle,
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

	sender, err := c.publisher.PeerConnection().AddTrack(track)
	if err != nil {
		return
	}
	c.localTracks[ti.Sid] = track
	c.trackSenders[ti.Sid] = sender
	c.publisher.Negotiate(false)
	writer = NewTrackWriter(c.ctx, track, path)

	// write tracks only after ICE connectivity
	if c.iceConnected.Load() {
		err = writer.Start()
	} else {
		c.pendingTrackWriters = append(c.pendingTrackWriters, writer)
	}

	return
}

func (c *RTCClient) AddStaticTrack(mime string, id string, label string) (writer *TrackWriter, err error) {
	return c.AddStaticTrackWithCodec(webrtc.RTPCodecCapability{MimeType: mime}, id, label)
}

func (c *RTCClient) AddStaticTrackWithCodec(codec webrtc.RTPCodecCapability, id string, label string) (writer *TrackWriter, err error) {
	track, err := webrtc.NewTrackLocalStaticSample(codec, id, label)
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

	logger.Debugw("adding file track",
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

func (c *RTCClient) PublishData(data []byte, kind livekit.DataPacket_Kind) error {
	if err := c.ensurePublisherConnected(); err != nil {
		return err
	}

	dp := &livekit.DataPacket{
		Kind: kind,
		Value: &livekit.DataPacket_User{
			User: &livekit.UserPacket{Payload: data},
		},
	}
	payload, err := proto.Marshal(dp)
	if err != nil {
		return err
	}

	if kind == livekit.DataPacket_RELIABLE {
		return c.reliableDC.Send(payload)
	} else {
		return c.lossyDC.Send(payload)
	}
}

func (c *RTCClient) GetPublishedTrackIDs() []string {
	c.lock.Lock()
	defer c.lock.Unlock()
	var trackIDs []string
	for key := range c.localTracks {
		trackIDs = append(trackIDs, key)
	}
	return trackIDs
}

func (c *RTCClient) ensurePublisherConnected() error {
	if c.publisherConnected.Load() {
		return nil
	}

	if c.publisher.PeerConnection().ConnectionState() == webrtc.PeerConnectionStateNew {
		// start negotiating
		c.publisher.Negotiate(false)
	}

	dcOpen := atomic.NewBool(false)
	c.reliableDC.OnOpen(func() {
		dcOpen.Store(true)
	})
	if c.reliableDC.ReadyState() == webrtc.DataChannelStateOpen {
		dcOpen.Store(true)
	}

	// wait until connected, increase wait time since it takes more than 10s sometimes on GH
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("could not connect publisher after timeout")
		case <-time.After(10 * time.Millisecond):
			if c.publisherConnected.Load() && dcOpen.Load() {
				return nil
			}
		}
	}
}

func (c *RTCClient) handleDataMessage(msg webrtc.DataChannelMessage) {
	dp := &livekit.DataPacket{}
	err := proto.Unmarshal(msg.Data, dp)
	if err != nil {
		return
	}
	if val, ok := dp.Value.(*livekit.DataPacket_User); ok {
		if c.OnDataReceived != nil {
			c.OnDataReceived(val.User.Payload, val.User.ParticipantSid)
		}
	}
}

// handles a server initiated offer, handle on subscriber PC
func (c *RTCClient) handleOffer(desc webrtc.SessionDescription) error {
	if err := c.subscriber.SetRemoteDescription(desc); err != nil {
		return err
	}

	// if we received an offer, we'd have to answer
	answer, err := c.subscriber.PeerConnection().CreateAnswer(nil)
	if err != nil {
		return err
	}

	if err := c.subscriber.PeerConnection().SetLocalDescription(answer); err != nil {
		return err
	}

	// send remote an answer
	logger.Infow("sending subscriber answer",
		"participant", c.localParticipant.Identity,
		// "sdp", answer,
	)
	return c.SendRequest(&livekit.SignalRequest{
		Message: &livekit.SignalRequest_Answer{
			Answer: rtc.ToProtoSessionDescription(answer),
		},
	})
}

// the client handles answer on the publisher PC
func (c *RTCClient) handleAnswer(desc webrtc.SessionDescription) error {
	logger.Infow("handling server answer", "participant", c.localParticipant.Identity)
	// remote answered the offer, establish connection
	err := c.publisher.SetRemoteDescription(desc)
	if err != nil {
		return err
	}

	if c.connected.Swap(true) {
		// already connected
		return nil
	}
	return nil
}

func (c *RTCClient) onOffer(offer webrtc.SessionDescription) {
	if c.localParticipant != nil {
		logger.Infow("starting negotiation", "participant", c.localParticipant.Identity)
	}
	_ = c.SendRequest(&livekit.SignalRequest{
		Message: &livekit.SignalRequest_Offer{
			Offer: rtc.ToProtoSessionDescription(offer),
		},
	})
}

func (c *RTCClient) processTrack(track *webrtc.TrackRemote) {
	lastUpdate := time.Time{}
	pId, trackId := rtc.UnpackStreamID(track.StreamID())
	if trackId == "" {
		trackId = livekit.TrackID(track.ID())
	}
	c.lock.Lock()
	c.subscribedTracks[pId] = append(c.subscribedTracks[pId], track)
	c.lock.Unlock()

	logger.Infow("client added track", "participant", c.localParticipant.Identity,
		"pID", pId,
		"trackID", trackId,
	)

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
			logger.Warnw("error reading RTP", err)
			continue
		}
		c.lock.Lock()
		c.lastPackets[pId] = pkt
		c.bytesReceived[pId] += uint64(pkt.MarshalSize())
		c.lock.Unlock()
		numBytes += pkt.MarshalSize()
		if time.Since(lastUpdate) > 30*time.Second {
			logger.Infow("consumed from participant",
				"trackID", trackId, "pID", pId,
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

	_ = c.subscriber.PeerConnection().WriteRTCP(packets)
}
