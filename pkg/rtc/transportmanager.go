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

package rtc

import (
	"context"
	"io"
	"math/bits"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/sctp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
	"github.com/pkg/errors"
	"go.uber.org/atomic"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/mediatransportutil/pkg/twcc"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/rtc/transport"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/datachannel"
	"github.com/livekit/livekit-server/pkg/sfu/pacer"
	"github.com/livekit/livekit-server/pkg/telemetry"
)

const (
	failureCountThreshold     = 2
	preferNextByFailureWindow = time.Minute

	// when RR report loss percentage over this threshold, we consider it is a unstable event
	udpLossFracUnstable = 25
	// if in last 32 times RR, the unstable report count over this threshold, the connection is unstable
	udpLossUnstableCountThreshold = 20
)

// -------------------------------

type TransportManagerTransportHandler struct {
	transport.Handler
	t      *TransportManager
	logger logger.Logger
}

func (h TransportManagerTransportHandler) OnFailed(isShortLived bool, iceConnectionInfo *types.ICEConnectionInfo) {
	if isShortLived {
		h.logger.Infow("short ice connection", connectionDetailsFields([]*types.ICEConnectionInfo{iceConnectionInfo})...)
	}
	h.t.handleConnectionFailed(isShortLived)
	h.Handler.OnFailed(isShortLived, iceConnectionInfo)
}

// -------------------------------

type TransportManagerParams struct {
	SubscriberAsPrimary          bool
	UseSinglePeerConnection      bool
	Config                       *WebRTCConfig
	Twcc                         *twcc.Responder
	ProtocolVersion              types.ProtocolVersion
	CongestionControlConfig      config.CongestionControlConfig
	EnabledSubscribeCodecs       []*livekit.Codec
	EnabledPublishCodecs         []*livekit.Codec
	SimTracks                    map[uint32]SimulcastTrackInfo
	ClientInfo                   ClientInfo
	Migration                    bool
	AllowTCPFallback             bool
	TCPFallbackRTTThreshold      int
	AllowUDPUnstableFallback     bool
	TURNSEnabled                 bool
	AllowPlayoutDelay            bool
	DataChannelMaxBufferedAmount uint64
	DatachannelSlowThreshold     int
	Logger                       logger.Logger
	PublisherHandler             transport.Handler
	SubscriberHandler            transport.Handler
	DataChannelStats             *telemetry.BytesTrackStats
	UseOneShotSignallingMode     bool
	FireOnTrackBySdp             bool
}

type TransportManager struct {
	params TransportManagerParams

	lock sync.RWMutex

	publisher               *PCTransport
	subscriber              *PCTransport
	failureCount            int
	isTransportReconfigured bool
	lastFailure             time.Time
	lastSignalAt            time.Time
	signalSourceValid       atomic.Bool

	pendingOfferPublisher        *webrtc.SessionDescription
	pendingOfferIdPublisher      uint32
	pendingDataChannelsPublisher []*livekit.DataChannelInfo
	iceConfig                    *livekit.ICEConfig

	mediaLossProxy       *MediaLossProxy
	udpLossUnstableCount uint32
	signalingRTT, udpRTT uint32

	onICEConfigChanged func(iceConfig *livekit.ICEConfig)

	droppedBySlowReaderCount atomic.Uint32
}

func NewTransportManager(params TransportManagerParams) (*TransportManager, error) {
	if params.Logger == nil {
		params.Logger = logger.GetLogger()
	}
	t := &TransportManager{
		params:         params,
		mediaLossProxy: NewMediaLossProxy(MediaLossProxyParams{Logger: params.Logger}),
		iceConfig:      &livekit.ICEConfig{},
	}
	t.mediaLossProxy.OnMediaLossUpdate(t.onMediaLossUpdate)

	lgr := LoggerWithPCTarget(params.Logger, livekit.SignalTarget_PUBLISHER)
	publisher, err := NewPCTransport(TransportParams{
		ProtocolVersion:              params.ProtocolVersion,
		Config:                       params.Config,
		Twcc:                         params.Twcc,
		DirectionConfig:              params.Config.Publisher,
		CongestionControlConfig:      params.CongestionControlConfig,
		EnabledCodecs:                params.EnabledPublishCodecs,
		Logger:                       lgr,
		SimTracks:                    params.SimTracks,
		ClientInfo:                   params.ClientInfo,
		IsSendSide:                   params.UseOneShotSignallingMode || params.UseSinglePeerConnection,
		AllowPlayoutDelay:            params.AllowPlayoutDelay,
		Transport:                    livekit.SignalTarget_PUBLISHER,
		Handler:                      TransportManagerTransportHandler{params.PublisherHandler, t, lgr},
		UseOneShotSignallingMode:     params.UseOneShotSignallingMode,
		DataChannelMaxBufferedAmount: params.DataChannelMaxBufferedAmount,
		DatachannelSlowThreshold:     params.DatachannelSlowThreshold,
		FireOnTrackBySdp:             params.FireOnTrackBySdp,
	})
	if err != nil {
		return nil, err
	}
	t.publisher = publisher

	if !t.params.UseOneShotSignallingMode && !t.params.UseSinglePeerConnection {
		lgr := LoggerWithPCTarget(params.Logger, livekit.SignalTarget_SUBSCRIBER)
		subscriber, err := NewPCTransport(TransportParams{
			ProtocolVersion:              params.ProtocolVersion,
			Config:                       params.Config,
			DirectionConfig:              params.Config.Subscriber,
			CongestionControlConfig:      params.CongestionControlConfig,
			EnabledCodecs:                params.EnabledSubscribeCodecs,
			Logger:                       lgr,
			ClientInfo:                   params.ClientInfo,
			IsOfferer:                    true,
			IsSendSide:                   true,
			AllowPlayoutDelay:            params.AllowPlayoutDelay,
			DataChannelMaxBufferedAmount: params.DataChannelMaxBufferedAmount,
			DatachannelSlowThreshold:     params.DatachannelSlowThreshold,
			Transport:                    livekit.SignalTarget_SUBSCRIBER,
			Handler:                      TransportManagerTransportHandler{params.SubscriberHandler, t, lgr},
			FireOnTrackBySdp:             params.FireOnTrackBySdp,
		})
		if err != nil {
			return nil, err
		}
		t.subscriber = subscriber
	}
	if !t.params.Migration && t.params.SubscriberAsPrimary {
		if err := t.createDataChannelsForSubscriber(nil); err != nil {
			return nil, err
		}
	}

	t.signalSourceValid.Store(true)
	return t, nil
}

func (t *TransportManager) Close() {
	if t.publisher != nil {
		t.publisher.Close()
	}
	if t.subscriber != nil {
		t.subscriber.Close()
	}
}

func (t *TransportManager) SubscriberClose() {
	t.subscriber.Close()
}

func (t *TransportManager) HasPublisherEverConnected() bool {
	return t.publisher.HasEverConnected()
}

func (t *TransportManager) IsPublisherEstablished() bool {
	return t.publisher.IsEstablished()
}

func (t *TransportManager) GetPublisherRTT() (float64, bool) {
	return t.publisher.GetRTT()
}

func (t *TransportManager) GetPublisherMid(rtpReceiver *webrtc.RTPReceiver) string {
	return t.publisher.GetMid(rtpReceiver)
}

func (t *TransportManager) GetPublisherRTPTransceiver(mid string) *webrtc.RTPTransceiver {
	return t.publisher.GetRTPTransceiver(mid)
}

func (t *TransportManager) GetPublisherRTPReceiver(mid string) *webrtc.RTPReceiver {
	return t.publisher.GetRTPReceiver(mid)
}

func (t *TransportManager) WritePublisherRTCP(pkts []rtcp.Packet) error {
	return t.publisher.WriteRTCP(pkts)
}

func (t *TransportManager) GetSubscriberRTT() (float64, bool) {
	if t.params.UseOneShotSignallingMode || t.params.UseSinglePeerConnection {
		return t.publisher.GetRTT()
	} else {
		return t.subscriber.GetRTT()
	}
}

func (t *TransportManager) HasSubscriberEverConnected() bool {
	if t.params.UseOneShotSignallingMode || t.params.UseSinglePeerConnection {
		return t.publisher.HasEverConnected()
	} else {
		return t.subscriber.HasEverConnected()
	}
}

func (t *TransportManager) AddTrackLocal(
	trackLocal webrtc.TrackLocal,
	params types.AddTrackParams,
	enabledCodecs []*livekit.Codec,
	rtcpFeedbackConfig RTCPFeedbackConfig,
) (*webrtc.RTPSender, *webrtc.RTPTransceiver, error) {
	if t.params.UseOneShotSignallingMode || t.params.UseSinglePeerConnection {
		return t.publisher.AddTrack(trackLocal, params, enabledCodecs, rtcpFeedbackConfig)
	} else {
		return t.subscriber.AddTrack(trackLocal, params, enabledCodecs, rtcpFeedbackConfig)
	}
}

func (t *TransportManager) AddTransceiverFromTrackLocal(
	trackLocal webrtc.TrackLocal,
	params types.AddTrackParams,
	enabledCodecs []*livekit.Codec,
	rtcpFeedbackConfig RTCPFeedbackConfig,
) (*webrtc.RTPSender, *webrtc.RTPTransceiver, error) {
	if t.params.UseOneShotSignallingMode || t.params.UseSinglePeerConnection {
		return t.publisher.AddTransceiverFromTrack(trackLocal, params, enabledCodecs, rtcpFeedbackConfig)
	} else {
		return t.subscriber.AddTransceiverFromTrack(trackLocal, params, enabledCodecs, rtcpFeedbackConfig)
	}
}

func (t *TransportManager) RemoveTrackLocal(sender *webrtc.RTPSender) error {
	if t.params.UseOneShotSignallingMode || t.params.UseSinglePeerConnection {
		return t.publisher.RemoveTrack(sender)
	} else {
		return t.subscriber.RemoveTrack(sender)
	}
}

func (t *TransportManager) WriteSubscriberRTCP(pkts []rtcp.Packet) error {
	if t.params.UseOneShotSignallingMode || t.params.UseSinglePeerConnection {
		return t.publisher.WriteRTCP(pkts)
	} else {
		return t.subscriber.WriteRTCP(pkts)
	}
}

func (t *TransportManager) GetSubscriberPacer() pacer.Pacer {
	if t.params.UseOneShotSignallingMode || t.params.UseSinglePeerConnection {
		return t.publisher.GetPacer()
	} else {
		return t.subscriber.GetPacer()
	}
}

func (t *TransportManager) AddSubscribedTrack(subTrack types.SubscribedTrack) {
	if t.params.UseOneShotSignallingMode || t.params.UseSinglePeerConnection {
		t.publisher.AddTrackToStreamAllocator(subTrack)
	} else {
		t.subscriber.AddTrackToStreamAllocator(subTrack)
	}
}

func (t *TransportManager) RemoveSubscribedTrack(subTrack types.SubscribedTrack) {
	if t.params.UseOneShotSignallingMode || t.params.UseSinglePeerConnection {
		t.publisher.RemoveTrackFromStreamAllocator(subTrack)
	} else {
		t.subscriber.RemoveTrackFromStreamAllocator(subTrack)
	}
}

func (t *TransportManager) SendDataMessage(kind livekit.DataPacket_Kind, data []byte) error {
	// downstream data is sent via primary peer connection
	return t.handleSendDataResult(t.getTransport(true).SendDataMessage(kind, data), kind.String(), len(data))
}

func (t *TransportManager) SendDataMessageUnlabeled(data []byte, useRaw bool, sender livekit.ParticipantIdentity) error {
	// downstream data is sent via primary peer connection
	return t.handleSendDataResult(
		t.getTransport(true).SendDataMessageUnlabeled(data, useRaw, sender),
		"unlabeled",
		len(data),
	)
}

func (t *TransportManager) handleSendDataResult(err error, kind string, size int) error {
	if err != nil {
		if !utils.ErrorIsOneOf(
			err,
			io.ErrClosedPipe,
			sctp.ErrStreamClosed,
			ErrTransportFailure,
			ErrDataChannelBufferFull,
			context.DeadlineExceeded,
		) {
			if errors.Is(err, datachannel.ErrDataDroppedBySlowReader) {
				droppedBySlowReaderCount := t.droppedBySlowReaderCount.Inc()
				if (droppedBySlowReaderCount-1)%100 == 0 {
					t.params.Logger.Infow(
						"drop data message by slow reader",
						"error", err,
						"kind", kind,
						"count", droppedBySlowReaderCount,
					)
				}
			} else {
				t.params.Logger.Warnw("send data message error", err)
			}
		}
		if utils.ErrorIsOneOf(err, sctp.ErrStreamClosed, io.ErrClosedPipe) {
			if t.params.SubscriberAsPrimary {
				t.params.SubscriberHandler.OnDataSendError(err)
			} else {
				t.params.PublisherHandler.OnDataSendError(err)
			}
		}
	} else {
		t.params.DataChannelStats.AddBytes(uint64(size), true)
	}

	return err
}

func (t *TransportManager) createDataChannelsForSubscriber(pendingDataChannels []*livekit.DataChannelInfo) error {
	var (
		reliableID, lossyID       uint16
		reliableIDPtr, lossyIDPtr *uint16
	)

	//
	// For old version migration clients, they don't send subscriber data channel info
	// so we need to create data channels with default ID and don't negotiate as client already has
	// data channels with default ID.
	//
	// For new version migration clients, we create data channels with new ID and negotiate with client
	//
	for _, dc := range pendingDataChannels {
		if dc.Label == ReliableDataChannel {
			// pion use step 2 for auto generated ID, so we need to add 4 to avoid conflict
			reliableID = uint16(dc.Id) + 4
			reliableIDPtr = &reliableID
		} else if dc.Label == LossyDataChannel {
			lossyID = uint16(dc.Id) + 4
			lossyIDPtr = &lossyID
		}
	}

	ordered := true
	negotiated := t.params.Migration && reliableIDPtr == nil
	if err := t.subscriber.CreateDataChannel(ReliableDataChannel, &webrtc.DataChannelInit{
		Ordered:    &ordered,
		ID:         reliableIDPtr,
		Negotiated: &negotiated,
	}); err != nil {
		return err
	}

	ordered = false
	retransmits := uint16(0)
	negotiated = t.params.Migration && lossyIDPtr == nil
	if err := t.subscriber.CreateDataChannel(LossyDataChannel, &webrtc.DataChannelInit{
		Ordered:        &ordered,
		MaxRetransmits: &retransmits,
		ID:             lossyIDPtr,
		Negotiated:     &negotiated,
	}); err != nil {
		return err
	}
	return nil
}

func (t *TransportManager) GetUnmatchMediaForOffer(parsedOffer *sdp.SessionDescription, mediaType string) (unmatched []*sdp.MediaDescription, err error) {
	var lastMatchedMid string
	if lastAnswer := t.publisher.CurrentLocalDescription(); lastAnswer != nil {
		parsedAnswer, err1 := lastAnswer.Unmarshal()
		if err1 != nil {
			// should not happen
			t.params.Logger.Errorw("failed to parse last answer", err1)
			return unmatched, err1
		}

		for i := len(parsedAnswer.MediaDescriptions) - 1; i >= 0; i-- {
			media := parsedAnswer.MediaDescriptions[i]
			if media.MediaName.Media == mediaType {
				lastMatchedMid, _ = media.Attribute(sdp.AttrKeyMID)
				break
			}
		}
	}

	for i := len(parsedOffer.MediaDescriptions) - 1; i >= 0; i-- {
		media := parsedOffer.MediaDescriptions[i]
		if media.MediaName.Media == mediaType {
			mid, _ := media.Attribute(sdp.AttrKeyMID)
			if mid == lastMatchedMid {
				break
			}
			unmatched = append(unmatched, media)
		}
	}

	return
}

func (t *TransportManager) LastPublisherOffer() *webrtc.SessionDescription {
	return t.publisher.CurrentRemoteDescription()
}

func (t *TransportManager) LastPublisherOfferPending() *webrtc.SessionDescription {
	return t.publisher.PendingRemoteDescription()
}

func (t *TransportManager) HandleOffer(offer webrtc.SessionDescription, offerId uint32, shouldPend bool) error {
	t.lock.Lock()
	if shouldPend {
		t.pendingOfferPublisher = &offer
		t.pendingOfferIdPublisher = offerId
		t.lock.Unlock()
		return nil
	}
	t.lock.Unlock()

	return t.publisher.HandleRemoteDescription(offer, offerId)
}

func (t *TransportManager) GetAnswer() (webrtc.SessionDescription, uint32, error) {
	return t.publisher.GetAnswer()
}

func (t *TransportManager) GetPublisherICESessionUfrag() (string, error) {
	return t.publisher.GetICESessionUfrag()
}

func (t *TransportManager) HandleICETrickleSDPFragment(sdpFragment string) error {
	return t.publisher.HandleICETrickleSDPFragment(sdpFragment)
}

func (t *TransportManager) HandleICERestartSDPFragment(sdpFragment string) (string, error) {
	return t.publisher.HandleICERestartSDPFragment(sdpFragment)
}

func (t *TransportManager) ProcessPendingPublisherOffer() {
	t.lock.Lock()
	pendingOffer := t.pendingOfferPublisher
	t.pendingOfferPublisher = nil

	pendingOfferId := t.pendingOfferIdPublisher
	t.pendingOfferIdPublisher = 0
	t.lock.Unlock()

	if pendingOffer != nil {
		t.HandleOffer(*pendingOffer, pendingOfferId, false)
	}
}

func (t *TransportManager) HandleAnswer(answer webrtc.SessionDescription, answerId uint32) {
	t.subscriber.HandleRemoteDescription(answer, answerId)
}

// AddICECandidate adds candidates for remote peer
func (t *TransportManager) AddICECandidate(candidate webrtc.ICECandidateInit, target livekit.SignalTarget) {
	switch target {
	case livekit.SignalTarget_PUBLISHER:
		t.publisher.AddICECandidate(candidate)
	case livekit.SignalTarget_SUBSCRIBER:
		t.subscriber.AddICECandidate(candidate)
	default:
		err := errors.New("unknown signal target")
		t.params.Logger.Errorw("ice candidate for unknown signal target", err, "target", target)
	}
}

func (t *TransportManager) NegotiateSubscriber(force bool) {
	if t.subscriber != nil {
		t.subscriber.Negotiate(force)
	} else {
		t.publisher.Negotiate(force)
	}
}

func (t *TransportManager) HandleClientReconnect(reason livekit.ReconnectReason) {
	var (
		isShort              bool
		duration             time.Duration
		resetShortConnection bool
	)
	switch reason {
	case livekit.ReconnectReason_RR_PUBLISHER_FAILED:
		if t.publisher != nil {
			resetShortConnection = true
			isShort, duration = t.publisher.IsShortConnection(time.Now())
		}

	case livekit.ReconnectReason_RR_SUBSCRIBER_FAILED:
		if t.subscriber != nil {
			resetShortConnection = true
			isShort, duration = t.subscriber.IsShortConnection(time.Now())
		}
	}

	if isShort {
		t.lock.Lock()
		t.resetTransportConfigureLocked(false)
		t.lock.Unlock()
		t.params.Logger.Infow("short connection by client ice restart", "duration", duration, "reason", reason)
		t.handleConnectionFailed(isShort)
	}

	if resetShortConnection {
		if t.publisher != nil {
			t.publisher.ResetShortConnOnICERestart()
		}
		if t.subscriber != nil {
			t.subscriber.ResetShortConnOnICERestart()
		}
	}
}

func (t *TransportManager) ICERestart(iceConfig *livekit.ICEConfig) error {
	t.SetICEConfig(iceConfig)

	if t.subscriber != nil {
		return t.subscriber.ICERestart()
	}

	return nil
}

func (t *TransportManager) OnICEConfigChanged(f func(iceConfig *livekit.ICEConfig)) {
	t.lock.Lock()
	t.onICEConfigChanged = f
	t.lock.Unlock()
}

func (t *TransportManager) SetICEConfig(iceConfig *livekit.ICEConfig) {
	if iceConfig != nil {
		t.configureICE(iceConfig, true)
	}
}

func (t *TransportManager) GetICEConfig() *livekit.ICEConfig {
	t.lock.RLock()
	defer t.lock.RUnlock()
	if t.iceConfig == nil {
		return nil
	}
	return utils.CloneProto(t.iceConfig)
}

func (t *TransportManager) resetTransportConfigureLocked(reconfigured bool) {
	t.failureCount = 0
	t.isTransportReconfigured = reconfigured
	t.udpLossUnstableCount = 0
	t.lastFailure = time.Time{}
}

func (t *TransportManager) configureICE(iceConfig *livekit.ICEConfig, reset bool) {
	t.lock.Lock()
	isEqual := proto.Equal(t.iceConfig, iceConfig)
	if reset || !isEqual {
		t.resetTransportConfigureLocked(!reset)
	}

	if isEqual {
		t.lock.Unlock()
		return
	}

	t.params.Logger.Infow("setting ICE config", "iceConfig", logger.Proto(iceConfig))
	onICEConfigChanged := t.onICEConfigChanged
	t.iceConfig = iceConfig
	t.lock.Unlock()

	if iceConfig.PreferenceSubscriber != livekit.ICECandidateType_ICT_NONE {
		t.mediaLossProxy.OnMediaLossUpdate(nil)
	}

	if t.publisher != nil {
		t.publisher.SetPreferTCP(iceConfig.PreferencePublisher == livekit.ICECandidateType_ICT_TCP)
	}
	if t.subscriber != nil {
		t.subscriber.SetPreferTCP(iceConfig.PreferenceSubscriber == livekit.ICECandidateType_ICT_TCP)
	}

	if onICEConfigChanged != nil {
		onICEConfigChanged(iceConfig)
	}
}

func (t *TransportManager) SubscriberAsPrimary() bool {
	return t.params.SubscriberAsPrimary
}

func (t *TransportManager) GetICEConnectionInfo() []*types.ICEConnectionInfo {
	infos := make([]*types.ICEConnectionInfo, 0, 2)
	for _, pc := range []*PCTransport{t.publisher, t.subscriber} {
		if pc == nil {
			continue
		}

		info := pc.GetICEConnectionInfo()
		if info.HasCandidates() {
			infos = append(infos, info)
		}
	}
	return infos
}

func (t *TransportManager) getTransport(isPrimary bool) *PCTransport {
	switch {
	case t.publisher == nil:
		return t.subscriber

	case t.subscriber == nil:
		return t.publisher

	default:
		pcTransport := t.publisher
		if (isPrimary && t.params.SubscriberAsPrimary) || (!isPrimary && !t.params.SubscriberAsPrimary) {
			pcTransport = t.subscriber
		}

		return pcTransport
	}
}

func (t *TransportManager) getLowestPriorityConnectionType() types.ICEConnectionType {
	switch {
	case t.publisher == nil:
		return t.subscriber.GetICEConnectionType()

	case t.subscriber == nil:
		return t.publisher.GetICEConnectionType()

	default:
		ctype := t.publisher.GetICEConnectionType()
		if stype := t.subscriber.GetICEConnectionType(); stype > ctype {
			ctype = stype
		}
		return ctype
	}
}

func (t *TransportManager) handleConnectionFailed(isShortLived bool) {
	if !t.params.AllowTCPFallback || t.params.UseOneShotSignallingMode {
		return
	}

	t.lock.Lock()
	if t.isTransportReconfigured {
		t.lock.Unlock()
		return
	}

	lastSignalSince := time.Since(t.lastSignalAt)
	signalValid := t.signalSourceValid.Load()
	if !t.hasRecentSignalLocked() || !signalValid {
		// the failed might cause by network interrupt because signal closed or we have not seen any signal in the time window,
		// so don't switch to next candidate type
		t.params.Logger.Debugw(
			"ignoring prefer candidate check by ICE failure because signal connection interrupted",
			"lastSignalSince", lastSignalSince,
			"signalValid", signalValid,
		)
		t.failureCount = 0
		t.lastFailure = time.Time{}
		t.lock.Unlock()
		return
	}

	lowestPriorityConnectionType := t.getLowestPriorityConnectionType()

	//
	// Checking only `PreferenceSubscriber` field although any connection failure (PUBLISHER OR SUBSCRIBER) will
	// flow through here.
	//
	// As both transports are switched to the same type on any failure, checking just subscriber should be fine.
	//
	getNext := func(ic *livekit.ICEConfig) livekit.ICECandidateType {
		switch lowestPriorityConnectionType {
		case types.ICEConnectionTypeUDP:
			// try ICE/TCP if ICE/UDP failed
			if ic.PreferenceSubscriber == livekit.ICECandidateType_ICT_NONE {
				if t.params.ClientInfo.SupportsICETCP() && t.canUseICETCP() {
					return livekit.ICECandidateType_ICT_TCP
				} else if t.params.TURNSEnabled {
					// fallback to TURN/TLS if TCP is not supported
					return livekit.ICECandidateType_ICT_TLS
				}
			}

		case types.ICEConnectionTypeTCP:
			// try TURN/TLS if ICE/TCP failed,
			// the configuration could have been ICT_NONE or ICT_TCP,
			// in either case, fallback to TURN/TLS
			if t.params.TURNSEnabled {
				return livekit.ICECandidateType_ICT_TLS
			} else {
				// keep the current config
				return ic.PreferenceSubscriber
			}

		case types.ICEConnectionTypeTURN:
			// TURN/TLS is the most permissive option, if that fails there is nowhere to go to
			// the configuration could have been ICT_NONE or ICT_TLS,
			// keep the current config
			return ic.PreferenceSubscriber
		}
		return livekit.ICECandidateType_ICT_NONE
	}

	var preferNext livekit.ICECandidateType
	if isShortLived {
		preferNext = getNext(t.iceConfig)
	} else {
		t.failureCount++
		lastFailure := t.lastFailure
		t.lastFailure = time.Now()
		if t.failureCount < failureCountThreshold || time.Since(lastFailure) > preferNextByFailureWindow {
			t.lock.Unlock()
			return
		}

		preferNext = getNext(t.iceConfig)
	}

	if preferNext == t.iceConfig.PreferenceSubscriber {
		t.lock.Unlock()
		return
	}

	t.isTransportReconfigured = true
	t.lock.Unlock()

	switch preferNext {
	case livekit.ICECandidateType_ICT_TCP:
		t.params.Logger.Debugw("prefer TCP transport on both peer connections")

	case livekit.ICECandidateType_ICT_TLS:
		t.params.Logger.Debugw("prefer TLS transport both peer connections")

	case livekit.ICECandidateType_ICT_NONE:
		t.params.Logger.Debugw("allowing all transports on both peer connections")
	}

	// irrespective of which one fails, force prefer candidate on both as the other one might
	// fail at a different time and cause another disruption
	t.configureICE(&livekit.ICEConfig{
		PreferenceSubscriber: preferNext,
		PreferencePublisher:  preferNext,
	}, false)
}

func (t *TransportManager) SetMigrateInfo(
	previousOffer *webrtc.SessionDescription,
	previousAnswer *webrtc.SessionDescription,
	dataChannels []*livekit.DataChannelInfo,
) {
	t.lock.Lock()
	t.pendingDataChannelsPublisher = make([]*livekit.DataChannelInfo, 0, len(dataChannels))
	pendingDataChannelsSubscriber := make([]*livekit.DataChannelInfo, 0, len(dataChannels))
	for _, dci := range dataChannels {
		if dci.Target == livekit.SignalTarget_SUBSCRIBER {
			pendingDataChannelsSubscriber = append(pendingDataChannelsSubscriber, dci)
		} else {
			t.pendingDataChannelsPublisher = append(t.pendingDataChannelsPublisher, dci)
		}
	}
	t.lock.Unlock()

	if t.params.SubscriberAsPrimary {
		if err := t.createDataChannelsForSubscriber(pendingDataChannelsSubscriber); err != nil {
			t.params.Logger.Errorw("create subscriber data channels during migration failed", err)
		}
	}

	if t.params.UseSinglePeerConnection {
		t.publisher.SetPreviousSdp(previousAnswer, previousOffer)
	} else {
		t.subscriber.SetPreviousSdp(previousOffer, previousAnswer)
	}
}

func (t *TransportManager) ProcessPendingPublisherDataChannels() {
	t.lock.Lock()
	pendingDataChannels := t.pendingDataChannelsPublisher
	t.pendingDataChannelsPublisher = nil
	t.lock.Unlock()

	ordered := true
	negotiated := true

	for _, ci := range pendingDataChannels {
		var (
			dcLabel    string
			dcID       uint16
			dcExisting bool
			err        error
		)
		if ci.Label == LossyDataChannel {
			ordered = false
			retransmits := uint16(0)
			id := uint16(ci.GetId())
			dcLabel, dcID, dcExisting, err = t.publisher.CreateDataChannelIfEmpty(LossyDataChannel, &webrtc.DataChannelInit{
				Ordered:        &ordered,
				MaxRetransmits: &retransmits,
				Negotiated:     &negotiated,
				ID:             &id,
			})
		} else if ci.Label == ReliableDataChannel {
			id := uint16(ci.GetId())
			dcLabel, dcID, dcExisting, err = t.publisher.CreateDataChannelIfEmpty(ReliableDataChannel, &webrtc.DataChannelInit{
				Ordered:    &ordered,
				Negotiated: &negotiated,
				ID:         &id,
			})
		}
		if err != nil {
			t.params.Logger.Errorw("create migrated data channel failed", err, "label", ci.Label)
		} else if dcExisting {
			t.params.Logger.Debugw("existing data channel during migration", "label", dcLabel, "id", dcID)
		} else {
			t.params.Logger.Debugw("create migrated data channel", "label", dcLabel, "id", dcID)
		}
	}
}

func (t *TransportManager) HandleReceiverReport(dt *sfu.DownTrack, report *rtcp.ReceiverReport) {
	t.mediaLossProxy.HandleMaxLossFeedback(dt, report)
}

func (t *TransportManager) onMediaLossUpdate(loss uint8) {
	if t.params.TCPFallbackRTTThreshold == 0 || !t.params.AllowUDPUnstableFallback {
		return
	}
	t.lock.Lock()
	t.udpLossUnstableCount <<= 1
	if loss >= uint8(255*udpLossFracUnstable/100) {
		t.udpLossUnstableCount |= 1
		if bits.OnesCount32(t.udpLossUnstableCount) >= udpLossUnstableCountThreshold {
			if t.udpRTT > 0 && t.signalingRTT < uint32(float32(t.udpRTT)*1.3) && int(t.signalingRTT) < t.params.TCPFallbackRTTThreshold && t.hasRecentSignalLocked() {
				t.udpLossUnstableCount = 0
				t.lock.Unlock()

				t.params.Logger.Infow("udp connection unstable, switch to tcp", "signalingRTT", t.signalingRTT)
				if t.params.UseSinglePeerConnection {
					t.params.PublisherHandler.OnFailed(true, t.publisher.GetICEConnectionInfo())
				} else {
					t.params.SubscriberHandler.OnFailed(true, t.subscriber.GetICEConnectionInfo())
				}
				return
			}
		}
	}
	t.lock.Unlock()
}

func (t *TransportManager) UpdateSignalingRTT(rtt uint32) {
	t.lock.Lock()
	t.signalingRTT = rtt
	t.lock.Unlock()
	if t.publisher != nil {
		t.publisher.SetSignalingRTT(rtt)
	}
	if t.subscriber != nil {
		t.subscriber.SetSignalingRTT(rtt)
	}

	// TODO: considering using tcp rtt to calculate ice connection cost, if ice connection can't be established
	// within 5 * tcp rtt(at least 5s), means udp traffic might be block/dropped, switch to tcp.
	// Currently, most cases reported is that ice connected but subsequent connection, so left the thinking for now.
}

func (t *TransportManager) UpdateMediaRTT(rtt uint32) {
	t.lock.Lock()
	if t.udpRTT == 0 {
		t.udpRTT = rtt
	} else {
		t.udpRTT = uint32(int(t.udpRTT) + (int(rtt)-int(t.udpRTT))/2)
	}
	t.lock.Unlock()
}

func (t *TransportManager) UpdateLastSeenSignal() {
	t.lock.Lock()
	t.lastSignalAt = time.Now()
	t.lock.Unlock()
}

func (t *TransportManager) SinceLastSignal() time.Duration {
	t.lock.RLock()
	defer t.lock.RUnlock()
	return time.Since(t.lastSignalAt)
}

func (t *TransportManager) LastSeenSignalAt() time.Time {
	t.lock.RLock()
	defer t.lock.RUnlock()
	return t.lastSignalAt
}

func (t *TransportManager) canUseICETCP() bool {
	return t.params.TCPFallbackRTTThreshold == 0 || int(t.signalingRTT) < t.params.TCPFallbackRTTThreshold
}

func (t *TransportManager) SetSignalSourceValid(valid bool) {
	t.signalSourceValid.Store(valid)
	t.params.Logger.Debugw("signal source valid", "valid", valid)
}

func (t *TransportManager) SetSubscriberAllowPause(allowPause bool) {
	if t.params.UseOneShotSignallingMode || t.params.UseSinglePeerConnection {
		t.publisher.SetAllowPauseOfStreamAllocator(allowPause)
	} else {
		t.subscriber.SetAllowPauseOfStreamAllocator(allowPause)
	}
}

func (t *TransportManager) SetSubscriberChannelCapacity(channelCapacity int64) {
	if t.params.UseOneShotSignallingMode || t.params.UseSinglePeerConnection {
		t.publisher.SetChannelCapacityOfStreamAllocator(channelCapacity)
	} else {
		t.subscriber.SetChannelCapacityOfStreamAllocator(channelCapacity)
	}
}

func (t *TransportManager) hasRecentSignalLocked() bool {
	return time.Since(t.lastSignalAt) < PingTimeoutSeconds*time.Second
}
