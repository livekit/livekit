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

package client

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/thoas/go-funk"
	"go.uber.org/atomic"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/signalling"

	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/pkg/rtc/transport/transportfakes"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/mime"
)

type SignalRequestHandler func(msg *livekit.SignalRequest) error
type SignalRequestInterceptor func(msg *livekit.SignalRequest, next SignalRequestHandler) error
type SignalResponseHandler func(msg *livekit.SignalResponse) error
type SignalResponseInterceptor func(msg *livekit.SignalResponse, next SignalResponseHandler) error

type RTCClient struct {
	useSinglePeerConnection bool
	id                      livekit.ParticipantID
	conn                    *websocket.Conn
	publisher               *rtc.PCTransport
	subscriber              *rtc.PCTransport
	// sid => track
	localTracks        map[string]webrtc.TrackLocal
	trackSenders       map[string]*webrtc.RTPSender
	lock               sync.Mutex
	wsLock             sync.Mutex
	ctx                context.Context
	cancel             context.CancelFunc
	me                 *webrtc.MediaEngine // optional, populated only when receiving tracks
	subscribedTracks   map[livekit.ParticipantID][]*webrtc.TrackRemote
	localParticipant   *livekit.ParticipantInfo
	remoteParticipants map[livekit.ParticipantID]*livekit.ParticipantInfo

	signalRequestInterceptor  SignalRequestInterceptor
	signalResponseInterceptor SignalResponseInterceptor

	icQueue [2]atomic.Pointer[webrtc.ICECandidate]

	subscriberAsPrimary        atomic.Bool
	publisherFullyEstablished  atomic.Bool
	subscriberFullyEstablished atomic.Bool
	pongReceivedAt             atomic.Int64

	// tracks waiting to be acked, cid => trackInfo
	pendingPublishedTracks map[string]*livekit.TrackInfo

	// remote tracks waiting to be processed
	pendingRemoteTracks []*webrtc.TrackRemote

	pendingTrackWriters     []*TrackWriter
	OnConnected             func()
	OnDataReceived          func(data []byte, sid string)
	OnDataUnlabeledReceived func(data []byte)
	refreshToken            string

	// map of livekit.ParticipantID and last packet
	lastPackets   map[livekit.ParticipantID]*rtp.Packet
	bytesReceived map[livekit.ParticipantID]uint64

	subscriptionResponse atomic.Pointer[livekit.SubscriptionResponse]
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
		".ivf":  mime.MimeTypeVP8.String(),
		".h264": mime.MimeTypeH264.String(),
		".ogg":  mime.MimeTypeOpus.String(),
	}
)

type Options struct {
	AutoSubscribe             bool
	Publish                   string
	Attributes                map[string]string
	ClientInfo                *livekit.ClientInfo
	DisabledCodecs            []webrtc.RTPCodecCapability
	TokenCustomizer           func(token *auth.AccessToken, grants *auth.VideoGrant)
	SignalRequestInterceptor  SignalRequestInterceptor
	SignalResponseInterceptor SignalResponseInterceptor
	UseJoinRequestQueryParam  bool
}

func NewWebSocketConn(host, token string, opts *Options) (*websocket.Conn, error) {
	u, err := url.Parse(host + "/rtc")
	if err != nil {
		return nil, err
	}
	requestHeader := make(http.Header)
	SetAuthorizationToken(requestHeader, token)

	connectUrl := u.String()
	if opts != nil && opts.UseJoinRequestQueryParam {
		clientInfo := &livekit.ClientInfo{
			Os:       runtime.GOOS,
			Sdk:      livekit.ClientInfo_GO,
			Protocol: int32(types.CurrentProtocol),
		}
		if opts.ClientInfo != nil {
			clientInfo = opts.ClientInfo
		}

		connectionSettings := &livekit.ConnectionSettings{
			AutoSubscribe: opts.AutoSubscribe,
		}

		joinRequest := &livekit.JoinRequest{
			ClientInfo:            clientInfo,
			ConnectionSettings:    connectionSettings,
			ParticipantAttributes: opts.Attributes,
		}

		if marshalled, err := proto.Marshal(joinRequest); err == nil {
			wrapped := &livekit.WrappedJoinRequest{
				JoinRequest: marshalled,
			}
			if marshalled, err := proto.Marshal(wrapped); err == nil {
				connectUrl += fmt.Sprintf("?join_request=%s", base64.URLEncoding.EncodeToString(marshalled))
			}
		}
	} else {
		connectUrl += fmt.Sprintf("?protocol=%d", types.CurrentProtocol)

		sdk := "go"
		if opts != nil {
			connectUrl += fmt.Sprintf("&auto_subscribe=%t", opts.AutoSubscribe)
			if opts.Publish != "" {
				connectUrl += encodeQueryParam("publish", opts.Publish)
			}
			if len(opts.Attributes) != 0 {
				data, err := json.Marshal(opts.Attributes)
				if err != nil {
					return nil, err
				}
				connectUrl += encodeQueryParam("attributes", base64.URLEncoding.EncodeToString(data))
			}
			if opts.ClientInfo != nil {
				if opts.ClientInfo.DeviceModel != "" {
					connectUrl += encodeQueryParam("device_model", opts.ClientInfo.DeviceModel)
				}
				if opts.ClientInfo.Os != "" {
					connectUrl += encodeQueryParam("os", opts.ClientInfo.Os)
				}
				if opts.ClientInfo.Sdk != livekit.ClientInfo_UNKNOWN {
					sdk = opts.ClientInfo.Sdk.String()
				}
			}
		}
		connectUrl += encodeQueryParam("sdk", sdk)
	}

	conn, _, err := websocket.DefaultDialer.Dial(connectUrl, requestHeader)
	return conn, err
}

func SetAuthorizationToken(header http.Header, token string) {
	header.Set("Authorization", "Bearer "+token)
}

func NewRTCClient(conn *websocket.Conn, useSinglePeerConnection bool, opts *Options) (*RTCClient, error) {
	var err error

	c := &RTCClient{
		useSinglePeerConnection: useSinglePeerConnection,
		conn:                    conn,
		localTracks:             make(map[string]webrtc.TrackLocal),
		trackSenders:            make(map[string]*webrtc.RTPSender),
		pendingPublishedTracks:  make(map[string]*livekit.TrackInfo),
		subscribedTracks:        make(map[livekit.ParticipantID][]*webrtc.TrackRemote),
		remoteParticipants:      make(map[livekit.ParticipantID]*livekit.ParticipantInfo),
		me:                      &webrtc.MediaEngine{},
		lastPackets:             make(map[livekit.ParticipantID]*rtp.Packet),
		bytesReceived:           make(map[livekit.ParticipantID]uint64),
	}
	c.ctx, c.cancel = context.WithCancel(context.Background())

	conf := rtc.WebRTCConfig{
		WebRTCConfig: rtcconfig.WebRTCConfig{
			Configuration: rtcConf,
		},
	}
	conf.SettingEngine.SetLite(false)
	conf.SettingEngine.SetAnsweringDTLSRole(webrtc.DTLSRoleClient)
	ff := buffer.NewFactoryOfBufferFactory(500, 200)
	conf.SetBufferFactory(ff.CreateBufferFactory())
	var codecs []*livekit.Codec
	for _, codec := range []*livekit.Codec{
		{
			Mime: "audio/opus",
		},
		{
			Mime: "video/vp8",
		},
		{
			Mime: "video/h264",
		},
	} {
		var disabled bool
		if opts != nil {
			for _, dc := range opts.DisabledCodecs {
				if mime.IsMimeTypeStringEqual(dc.MimeType, codec.Mime) && (dc.SDPFmtpLine == "" || dc.SDPFmtpLine == codec.FmtpLine) {
					disabled = true
					break
				}
			}
		}
		if !disabled {
			codecs = append(codecs, codec)
		}
	}

	//
	// The signal targets are from point of view of server.
	// From client side, they are flipped,
	// i. e. the publisher transport on client side has SUBSCRIBER signal target (i. e. publisher is offerer).
	// Same applies for subscriber transport also
	//
	publisherHandler := &transportfakes.FakeHandler{}
	c.publisher, err = rtc.NewPCTransport(rtc.TransportParams{
		Config:                           &conf,
		DirectionConfig:                  conf.Subscriber,
		EnabledCodecs:                    codecs,
		IsOfferer:                        true,
		IsSendSide:                       true,
		Handler:                          publisherHandler,
		DatachannelMaxReceiverBufferSize: 1500,
		DatachannelSlowThreshold:         1024 * 1024 * 1024,
		FireOnTrackBySdp:                 true,
	})
	if err != nil {
		return nil, err
	}

	publisherHandler.OnICECandidateCalls(func(ic *webrtc.ICECandidate, t livekit.SignalTarget) error {
		return c.SendIceCandidate(ic, livekit.SignalTarget_PUBLISHER)
	})
	publisherHandler.OnTrackCalls(func(track *webrtc.TrackRemote, rtpReceiver *webrtc.RTPReceiver) {
		go c.processRemoteTrack(track)
	})
	publisherHandler.OnDataMessageCalls(c.handleDataMessage)
	publisherHandler.OnDataMessageUnlabeledCalls(c.handleDataMessageUnlabeled)
	publisherHandler.OnInitialConnectedCalls(func() {
		logger.Debugw("publisher initial connected", "participant", c.localParticipant.Identity)

		c.lock.Lock()
		defer c.lock.Unlock()
		for _, tw := range c.pendingTrackWriters {
			if err := tw.Start(); err != nil {
				logger.Errorw("track writer error", err)
			}
		}

		c.pendingTrackWriters = nil

		if c.OnConnected != nil {
			go c.OnConnected()
		}
	})
	publisherHandler.OnOfferCalls(c.onOffer)
	publisherHandler.OnFullyEstablishedCalls(func() {
		logger.Debugw("publisher fully established", "participant", c.localParticipant.Identity, "pID", c.localParticipant.Sid)
		c.publisherFullyEstablished.Store(true)
	})

	ordered := true
	if err := c.publisher.CreateDataChannel(rtc.ReliableDataChannel, &webrtc.DataChannelInit{
		Ordered: &ordered,
	}); err != nil {
		return nil, err
	}

	ordered = false
	maxRetransmits := uint16(0)
	if err := c.publisher.CreateDataChannel(rtc.LossyDataChannel, &webrtc.DataChannelInit{
		Ordered:        &ordered,
		MaxRetransmits: &maxRetransmits,
	}); err != nil {
		return nil, err
	}

	if err := c.publisher.CreateDataChannel("pubraw", &webrtc.DataChannelInit{
		Ordered: &ordered,
	}); err != nil {
		return nil, err
	}

	if !c.useSinglePeerConnection {
		subscriberHandler := &transportfakes.FakeHandler{}
		c.subscriber, err = rtc.NewPCTransport(rtc.TransportParams{
			Config:                           &conf,
			DirectionConfig:                  conf.Publisher,
			EnabledCodecs:                    codecs,
			Handler:                          subscriberHandler,
			DatachannelMaxReceiverBufferSize: 1500,
			DatachannelSlowThreshold:         1024 * 1024 * 1024,
			FireOnTrackBySdp:                 true,
		})
		if err != nil {
			return nil, err
		}

		ordered := false
		if err := c.subscriber.CreateReadableDataChannel("subraw", &webrtc.DataChannelInit{
			Ordered: &ordered,
		}); err != nil {
			return nil, err
		}

		subscriberHandler.OnICECandidateCalls(func(ic *webrtc.ICECandidate, t livekit.SignalTarget) error {
			if ic == nil {
				return nil
			}
			return c.SendIceCandidate(ic, livekit.SignalTarget_SUBSCRIBER)
		})
		subscriberHandler.OnTrackCalls(func(track *webrtc.TrackRemote, rtpReceiver *webrtc.RTPReceiver) {
			go c.processRemoteTrack(track)
		})
		subscriberHandler.OnDataMessageCalls(c.handleDataMessage)
		subscriberHandler.OnDataMessageUnlabeledCalls(c.handleDataMessageUnlabeled)
		subscriberHandler.OnInitialConnectedCalls(func() {
			logger.Debugw("subscriber initial connected", "participant", c.localParticipant.Identity)

			c.lock.Lock()
			defer c.lock.Unlock()
			for _, tw := range c.pendingTrackWriters {
				if err := tw.Start(); err != nil {
					logger.Errorw("track writer error", err)
				}
			}

			c.pendingTrackWriters = nil

			if c.OnConnected != nil {
				go c.OnConnected()
			}
		})
		subscriberHandler.OnFullyEstablishedCalls(func() {
			logger.Debugw("subscriber fully established", "participant", c.localParticipant.Identity, "pID", c.localParticipant.Sid)
			c.subscriberFullyEstablished.Store(true)
		})
		subscriberHandler.OnAnswerCalls(func(answer webrtc.SessionDescription, answerId uint32, _midToTrackID map[string]string) error {
			// send remote an answer
			logger.Infow(
				"sending subscriber answer",
				"participant", c.localParticipant.Identity,
				"sdp", answer,
			)
			return c.SendRequest(&livekit.SignalRequest{
				Message: &livekit.SignalRequest_Answer{
					Answer: signalling.ToProtoSessionDescription(answer, answerId),
				},
			})
		})
	}

	if opts != nil {
		c.signalRequestInterceptor = opts.SignalRequestInterceptor
		c.signalResponseInterceptor = opts.SignalResponseInterceptor
	}

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
		if errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			logger.Errorw("error while reading", err)
			return err
		}
		if c.signalResponseInterceptor != nil {
			err = c.signalResponseInterceptor(res, c.handleSignalResponse)
		} else {
			err = c.handleSignalResponse(res)
		}
		if err != nil {
			return err
		}
	}
}

func (c *RTCClient) handleSignalResponse(res *livekit.SignalResponse) error {
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
			c.subscriberAsPrimary.Store(false)
			c.publisher.Negotiate(false)
		} else {
			c.subscriberAsPrimary.Store(true)
		}

		logger.Infow("join accepted, awaiting offer", "participant", msg.Join.Participant.Identity)
	case *livekit.SignalResponse_Answer:
		logger.Infow(
			"received server answer",
			"participant", c.localParticipant.Identity,
			"answer", msg.Answer.Sdp,
		)
		c.handleAnswer(signalling.FromProtoSessionDescription(msg.Answer))
	case *livekit.SignalResponse_MappedAnswer:
		logger.Infow(
			"received mapped server answer",
			"participant", c.localParticipant.Identity,
			"answer", msg.MappedAnswer.SessionDescription.Sdp,
		)
		c.handleAnswer(signalling.FromProtoSessionDescription(msg.MappedAnswer.SessionDescription))
	case *livekit.SignalResponse_Offer:
		desc, offerId := signalling.FromProtoSessionDescription(msg.Offer)
		logger.Infow(
			"received server offer",
			"participant", c.localParticipant.Identity,
			"sdp", desc,
			"offerId", offerId,
		)
		c.handleOffer(desc, offerId)
	case *livekit.SignalResponse_Trickle:
		candidateInit, err := signalling.FromProtoTrickle(msg.Trickle)
		if err != nil {
			return err
		}
		if msg.Trickle.Target == livekit.SignalTarget_PUBLISHER {
			c.publisher.AddICECandidate(candidateInit)
		} else {
			c.subscriber.AddICECandidate(candidateInit)
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
		if sender := c.trackSenders[sid]; sender != nil {
			if err := c.publisher.RemoveTrack(sender); err != nil {
				logger.Errorw("Could not unpublish track", err)
			}
			c.publisher.Negotiate(false)
		}
		delete(c.trackSenders, sid)
		delete(c.localTracks, sid)
		c.lock.Unlock()
	case *livekit.SignalResponse_Pong:
		c.pongReceivedAt.Store(msg.Pong)
	case *livekit.SignalResponse_SubscriptionResponse:
		c.subscriptionResponse.Store(msg.SubscriptionResponse)
	case *livekit.SignalResponse_MediaSectionsRequirement:
		logger.Infow(
			"received media sections requirement",
			"participant", c.localParticipant.Identity,
			"numAudios", msg.MediaSectionsRequirement.NumAudios,
			"numVideos", msg.MediaSectionsRequirement.NumVideos,
		)
		c.handleMediaSectionsRequirement(msg.MediaSectionsRequirement)
	}
	return nil
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
			if c.subscriberAsPrimary.Load() {
				if c.subscriberFullyEstablished.Load() {
					return nil
				}
			} else {
				if c.publisherFullyEstablished.Load() {
					return nil
				}
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
			Leave: &livekit.LeaveRequest{
				Reason: livekit.DisconnectReason_CLIENT_INITIATED,
				Action: livekit.LeaveRequest_DISCONNECT,
			},
		},
	})
	c.publisherFullyEstablished.Store(false)
	c.subscriberFullyEstablished.Store(false)
	_ = c.conn.Close()
	if c.publisher != nil {
		c.publisher.Close()
	}
	if c.subscriber != nil {
		c.subscriber.Close()
	}
	c.cancel()
}

func (c *RTCClient) RefreshToken() string {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.refreshToken
}

func (c *RTCClient) PongReceivedAt() int64 {
	return c.pongReceivedAt.Load()
}

func (c *RTCClient) GetSubscriptionResponseAndClear() *livekit.SubscriptionResponse {
	return c.subscriptionResponse.Swap(nil)
}

func (c *RTCClient) SendPing() error {
	return c.SendRequest(&livekit.SignalRequest{
		Message: &livekit.SignalRequest_Ping{
			Ping: time.Now().UnixNano(),
		},
	})
}

func (c *RTCClient) SendRequest(msg *livekit.SignalRequest) error {
	if c.signalRequestInterceptor != nil {
		return c.signalRequestInterceptor(msg, c.sendRequest)
	} else {
		return c.sendRequest(msg)
	}
}

func (c *RTCClient) sendRequest(msg *livekit.SignalRequest) error {
	payload, err := proto.Marshal(msg)
	if err != nil {
		return err
	}

	c.wsLock.Lock()
	defer c.wsLock.Unlock()
	return c.conn.WriteMessage(websocket.BinaryMessage, payload)
}

func (c *RTCClient) SendIceCandidate(ic *webrtc.ICECandidate, target livekit.SignalTarget) error {
	prevIC := c.icQueue[target].Swap(ic)
	if prevIC == nil {
		return nil
	}

	return c.SendRequest(&livekit.SignalRequest{
		Message: &livekit.SignalRequest_Trickle{
			Trickle: signalling.ToProtoTrickle(prevIC.ToJSON(), target, ic == nil),
		},
	})
}

func (c *RTCClient) SetAttributes(attrs map[string]string) error {
	return c.SendRequest(&livekit.SignalRequest{
		Message: &livekit.SignalRequest_UpdateMetadata{
			UpdateMetadata: &livekit.UpdateParticipantMetadata{
				Attributes: attrs,
			},
		},
	})
}

func (c *RTCClient) hasPrimaryEverConnected() bool {
	if c.subscriberAsPrimary.Load() {
		return c.subscriber.HasEverConnected()
	} else {
		return c.publisher.HasEverConnected()
	}
}

type AddTrackParams struct {
	NoWriter bool
}

type AddTrackOption func(params *AddTrackParams)

func AddTrackNoWriter() AddTrackOption {
	return func(params *AddTrackParams) {
		params.NoWriter = true
	}
}

func (c *RTCClient) AddTrack(track *webrtc.TrackLocalStaticSample, path string, opts ...AddTrackOption) (writer *TrackWriter, err error) {
	var params AddTrackParams
	for _, opt := range opts {
		opt(&params)
	}
	trackType := livekit.TrackType_AUDIO
	if track.Kind() == webrtc.RTPCodecTypeVideo {
		trackType = livekit.TrackType_VIDEO
	}

	sender, _, err := c.publisher.AddTrack(track, types.AddTrackParams{}, nil, rtc.RTCPFeedbackConfig{})
	if err != nil {
		logger.Errorw(
			"add track failed", err,
			"participant", c.localParticipant.Identity,
			"pID", c.localParticipant.Sid,
			"trackID", track.ID(),
		)
		return
	}

	if err = c.SendAddTrack(track.ID(), track.Codec().MimeType, track.StreamID(), trackType); err != nil {
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
			if ti != nil {
				delete(c.pendingPublishedTracks, track.ID())
				c.lock.Unlock()
				break
			}
			c.lock.Unlock()
			time.Sleep(50 * time.Millisecond)
		}
		if ti != nil {
			break
		}
	}

	c.lock.Lock()
	defer c.lock.Unlock()

	c.localTracks[ti.Sid] = track
	c.trackSenders[ti.Sid] = sender
	c.publisher.Negotiate(false)

	if !params.NoWriter {
		writer = NewTrackWriter(c.ctx, track, path)

		// write tracks only after connection established
		if c.hasPrimaryEverConnected() {
			err = writer.Start()
		} else {
			c.pendingTrackWriters = append(c.pendingTrackWriters, writer)
		}
	}

	return
}

func (c *RTCClient) AddStaticTrack(mime string, id string, label string, opts ...AddTrackOption) (writer *TrackWriter, err error) {
	return c.AddStaticTrackWithCodec(webrtc.RTPCodecCapability{MimeType: mime}, id, label, opts...)
}

func (c *RTCClient) AddStaticTrackWithCodec(codec webrtc.RTPCodecCapability, id string, label string, opts ...AddTrackOption) (writer *TrackWriter, err error) {
	track, err := webrtc.NewTrackLocalStaticSample(codec, id, label)
	if err != nil {
		return
	}

	return c.AddTrack(track, "", opts...)
}

func (c *RTCClient) AddFileTrack(path string, id string, label string) (writer *TrackWriter, err error) {
	// determine file mime
	mime, ok := extMimeMapping[filepath.Ext(path)]
	if !ok {
		return nil, fmt.Errorf("%s has an unsupported extension", filepath.Base(path))
	}

	logger.Debugw("adding file track", "mime", mime)

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
func (c *RTCClient) SendAddTrack(cid string, mimeType string, name string, trackType livekit.TrackType) error {
	return c.SendRequest(&livekit.SignalRequest{
		Message: &livekit.SignalRequest_AddTrack{
			AddTrack: &livekit.AddTrackRequest{
				Cid:  cid,
				Name: name,
				Type: trackType,
				SimulcastCodecs: []*livekit.SimulcastCodec{
					{
						Cid:   cid,
						Codec: mimeType,
					},
				},
			},
		},
	})
}

func (c *RTCClient) PublishData(data []byte, kind livekit.DataPacket_Kind) error {
	if err := c.ensurePublisherConnected(); err != nil {
		return err
	}

	dpData, err := proto.Marshal(&livekit.DataPacket{
		Value: &livekit.DataPacket_User{
			User: &livekit.UserPacket{Payload: data},
		},
	})
	if err != nil {
		return err
	}

	return c.publisher.SendDataMessage(kind, dpData)
}

func (c *RTCClient) PublishDataUnlabeled(data []byte) error {
	if err := c.ensurePublisherConnected(); err != nil {
		return err
	}

	return c.publisher.SendDataMessageUnlabeled(data, true, "test")
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

// LastAnswer return SDP of the last answer for the publisher connection
func (c *RTCClient) LastAnswer() *webrtc.SessionDescription {
	return c.publisher.CurrentRemoteDescription()
}

func (c *RTCClient) ensurePublisherConnected() error {
	if c.publisher.HasEverConnected() {
		return nil
	}

	// start negotiating
	c.publisher.Negotiate(false)

	// wait until connected, increase wait time since it takes more than 10s sometimes on GH
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("could not connect publisher after timeout")
		case <-time.After(10 * time.Millisecond):
			if c.publisherFullyEstablished.Load() {
				return nil
			}
		}
	}
}

func (c *RTCClient) handleDataMessage(kind livekit.DataPacket_Kind, data []byte) {
	dp := &livekit.DataPacket{}
	err := proto.Unmarshal(data, dp)
	if err != nil {
		return
	}
	dp.Kind = kind
	if val, ok := dp.Value.(*livekit.DataPacket_User); ok {
		if c.OnDataReceived != nil {
			c.OnDataReceived(val.User.Payload, val.User.ParticipantSid)
		}
	}
}

func (c *RTCClient) handleDataMessageUnlabeled(data []byte) {
	if c.OnDataUnlabeledReceived != nil {
		c.OnDataUnlabeledReceived(data)
	}
}

// handles a server initiated offer, handle on subscriber PC
func (c *RTCClient) handleOffer(desc webrtc.SessionDescription, offerId uint32) {
	logger.Infow("handling server offer", "participant", c.localParticipant.Identity)
	c.subscriber.HandleRemoteDescription(desc, offerId)
	c.processPendingRemoteTracks()
}

// the client handles answer on the publisher PC
func (c *RTCClient) handleAnswer(desc webrtc.SessionDescription, answerId uint32) {
	logger.Infow("handling server answer", "participant", c.localParticipant.Identity)

	// remote answered the offer, establish connection
	c.publisher.HandleRemoteDescription(desc, answerId)
	c.processPendingRemoteTracks()
}

// the client handles media sections requirement on the publisher PC
func (c *RTCClient) handleMediaSectionsRequirement(mediaSectionsRequirement *livekit.MediaSectionsRequirement) {
	addTransceivers := func(kind webrtc.RTPCodecType, count uint32) {
		for i := uint32(0); i < count; i++ {
			if _, err := c.publisher.AddTransceiverFromKind(
				kind,
				webrtc.RTPTransceiverInit{
					Direction: webrtc.RTPTransceiverDirectionRecvonly,
				},
			); err != nil {
				logger.Warnw(
					"could not add transceiver", err,
					"participant", c.localParticipant.Identity,
					"kind", kind,
				)
			} else {
				logger.Infow(
					"added transceiver of kind",
					"participant", c.localParticipant.Identity,
					"kind", kind,
				)
			}
		}
	}

	addTransceivers(webrtc.RTPCodecTypeAudio, mediaSectionsRequirement.NumAudios)
	addTransceivers(webrtc.RTPCodecTypeVideo, mediaSectionsRequirement.NumVideos)
	c.publisher.Negotiate(false)
}

func (c *RTCClient) onOffer(offer webrtc.SessionDescription, offerId uint32) error {
	if c.localParticipant != nil {
		logger.Infow("starting negotiation", "participant", c.localParticipant.Identity)
		logger.Infow(
			"sending publisher offer",
			"participant", c.localParticipant.Identity,
			"offer", offer,
		)
	}
	return c.SendRequest(&livekit.SignalRequest{
		Message: &livekit.SignalRequest_Offer{
			Offer: signalling.ToProtoSessionDescription(offer, offerId),
		},
	})
}

func (c *RTCClient) processPendingRemoteTracks() {
	c.lock.Lock()
	pendingRemoteTracks := c.pendingRemoteTracks
	c.pendingRemoteTracks = nil
	c.lock.Unlock()

	for _, pendingRemoteTrack := range pendingRemoteTracks {
		go c.processRemoteTrack(pendingRemoteTrack)
	}
}

func (c *RTCClient) processRemoteTrack(track *webrtc.TrackRemote) {
	lastUpdate := time.Time{}

	// because of FireOnTrackBySdp, it is possible get an empty streamID
	// if media comes before SDP, cache and try later
	streamID := track.StreamID()
	if streamID == "" {
		logger.Infow(
			"client caching track",
			"participant", c.localParticipant.Identity,
			"pID", c.ID(),
			"codec", track.Codec(),
			"ssrc", track.SSRC(),
		)
		c.lock.Lock()
		c.pendingRemoteTracks = append(c.pendingRemoteTracks, track)
		c.lock.Unlock()
		return
	}

	publisherID, trackID := rtc.UnpackStreamID(streamID)
	if trackID == "" {
		trackID = livekit.TrackID(track.ID())
	}
	c.lock.Lock()
	c.subscribedTracks[publisherID] = append(c.subscribedTracks[publisherID], track)
	c.lock.Unlock()

	logger.Infow(
		"client added track",
		"participant", c.localParticipant.Identity,
		"pID", c.ID(),
		"publisherID", publisherID,
		"trackID", trackID,
		"codec", track.Codec(),
		"ssrc", track.SSRC(),
	)

	defer func() {
		c.lock.Lock()
		c.subscribedTracks[publisherID] = funk.Without(c.subscribedTracks[publisherID], track).([]*webrtc.TrackRemote)
		c.lock.Unlock()
	}()

	numBytes := 0
	for {
		pkt, _, err := track.ReadRTP()
		if c.ctx.Err() != nil {
			break
		}
		if rtc.IsEOF(err) {
			logger.Infow(
				"client track removed",
				"participant", c.localParticipant.Identity,
				"pID", c.ID(),
				"publisherID", publisherID,
				"trackID", trackID,
				"codec", track.Codec(),
				"ssrc", track.SSRC(),
			)
			break
		}
		if err != nil {
			logger.Warnw("error reading RTP", err)
			continue
		}
		c.lock.Lock()
		c.lastPackets[publisherID] = pkt
		c.bytesReceived[publisherID] += uint64(pkt.MarshalSize())
		c.lock.Unlock()
		numBytes += pkt.MarshalSize()
		if time.Since(lastUpdate) > 30*time.Second {
			logger.Infow(
				"consumed from participant",
				"participant", c.localParticipant.Identity,
				"pID", c.ID(),
				"publisherID", publisherID,
				"trackID", trackID,
				"size", numBytes,
			)
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

	_ = c.subscriber.WriteRTCP(packets)
}

func encodeQueryParam(key, value string) string {
	return fmt.Sprintf("&%s=%s", url.QueryEscape(key), url.QueryEscape(value))
}
