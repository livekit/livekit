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

type TransportManagerPublisherTransportHandler struct {
	TransportManagerTransportHandler
}

func (h TransportManagerPublisherTransportHandler) OnAnswer(sd webrtc.SessionDescription) error {
	h.t.lastPublisherAnswer.Store(sd)
	return h.Handler.OnAnswer(sd)
}

// -------------------------------

type TransportManagerParams struct {
	Identity                     livekit.ParticipantIdentity
	SID                          livekit.ParticipantID
	SubscriberAsPrimary          bool
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
	pendingDataChannelsPublisher []*livekit.DataChannelInfo
	lastPublisherAnswer          atomic.Value
	lastPublisherOffer           atomic.Value
	iceConfig                    *livekit.ICEConfig

	mediaLossProxy       *MediaLossProxy
	udpLossUnstableCount uint32
	signalingRTT, udpRTT uint32

	onICEConfigChanged func(iceConfig *livekit.ICEConfig)
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
		ParticipantID:                params.SID,
		ParticipantIdentity:          params.Identity,
		ProtocolVersion:              params.ProtocolVersion,
		Config:                       params.Config,
		Twcc:                         params.Twcc,
		DirectionConfig:              params.Config.Publisher,
		CongestionControlConfig:      params.CongestionControlConfig,
		EnabledCodecs:                params.EnabledPublishCodecs,
		Logger:                       lgr,
		SimTracks:                    params.SimTracks,
		ClientInfo:                   params.ClientInfo,
		Transport:                    livekit.SignalTarget_PUBLISHER,
		Handler:                      TransportManagerPublisherTransportHandler{TransportManagerTransportHandler{params.PublisherHandler, t, lgr}},
		UseOneShotSignallingMode:     params.UseOneShotSignallingMode,
		DataChannelMaxBufferedAmount: params.DataChannelMaxBufferedAmount,
		DatachannelSlowThreshold:     params.DatachannelSlowThreshold,
		FireOnTrackBySdp:             params.FireOnTrackBySdp,
	})
	if err != nil {
		return nil, err
	}
	t.publisher = publisher

	lgr = LoggerWithPCTarget(params.Logger, livekit.SignalTarget_SUBSCRIBER)
	subscriber, err := NewPCTransport(TransportParams{
		ParticipantID:            params.SID,
		ParticipantIdentity:      params.Identity,
		ProtocolVersion:          params.ProtocolVersion,
		Config:                   params.Config,
		DirectionConfig:          params.Config.Subscriber,
		CongestionControlConfig:  params.CongestionControlConfig,
		EnabledCodecs:            params.EnabledSubscribeCodecs,
		Logger:                   lgr,
		ClientInfo:               params.ClientInfo,
		IsOfferer:                true,
		IsSendSide:               true,
		AllowPlayoutDelay:        params.AllowPlayoutDelay,
		DatachannelSlowThreshold: params.DatachannelSlowThreshold,
		Transport:                livekit.SignalTarget_SUBSCRIBER,
		Handler:                  TransportManagerTransportHandler{params.SubscriberHandler, t, lgr},
	})
	if err != nil {
		return nil, err
	}
	t.subscriber = subscriber
	if !t.params.Migration {
		if err := t.createDataChannelsForSubscriber(nil); err != nil {
			return nil, err
		}
	}

	t.signalSourceValid.Store(true)
	return t, nil
}

func (t *TransportManager) Close() {
	t.publisher.Close()
	t.subscriber.Close()
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

func (t *TransportManager) GetPublisherRTPReceiver(mid string) *webrtc.RTPReceiver {
	return t.publisher.GetRTPReceiver(mid)
}

func (t *TransportManager) WritePublisherRTCP(pkts []rtcp.Packet) error {
	return t.publisher.WriteRTCP(pkts)
}

func (t *TransportManager) GetSubscriberRTT() (float64, bool) {
	return t.subscriber.GetRTT()
}

func (t *TransportManager) HasSubscriberEverConnected() bool {
	return t.subscriber.HasEverConnected()
}

func (t *TransportManager) AddTrackLocal(
	trackLocal webrtc.TrackLocal,
	params types.AddTrackParams,
) (*webrtc.RTPSender, *webrtc.RTPTransceiver, error) {
	if t.params.UseOneShotSignallingMode {
		return t.publisher.AddTrack(trackLocal, params)
	} else {
		return t.subscriber.AddTrack(trackLocal, params)
	}
}

func (t *TransportManager) AddTransceiverFromTrackLocal(
	trackLocal webrtc.TrackLocal,
	params types.AddTrackParams,
) (*webrtc.RTPSender, *webrtc.RTPTransceiver, error) {
	if t.params.UseOneShotSignallingMode {
		return t.publisher.AddTransceiverFromTrack(trackLocal, params)
	} else {
		return t.subscriber.AddTransceiverFromTrack(trackLocal, params)
	}
}

func (t *TransportManager) RemoveTrackLocal(sender *webrtc.RTPSender) error {
	if t.params.UseOneShotSignallingMode {
		return t.publisher.RemoveTrack(sender)
	} else {
		return t.subscriber.RemoveTrack(sender)
	}
}

func (t *TransportManager) WriteSubscriberRTCP(pkts []rtcp.Packet) error {
	if t.params.UseOneShotSignallingMode {
		return t.publisher.WriteRTCP(pkts)
	} else {
		return t.subscriber.WriteRTCP(pkts)
	}
}

func (t *TransportManager) GetSubscriberPacer() pacer.Pacer {
	return t.subscriber.GetPacer()
}

func (t *TransportManager) AddSubscribedTrack(subTrack types.SubscribedTrack) {
	t.subscriber.AddTrackToStreamAllocator(subTrack)
}

func (t *TransportManager) RemoveSubscribedTrack(subTrack types.SubscribedTrack) {
	t.subscriber.RemoveTrackFromStreamAllocator(subTrack)
}

func (t *TransportManager) SendDataPacket(kind livekit.DataPacket_Kind, encoded []byte) error {
	// downstream data is sent via primary peer connection
	err := t.getTransport(true).SendDataPacket(kind, encoded)
	if err != nil {
		if !utils.ErrorIsOneOf(err, io.ErrClosedPipe, sctp.ErrStreamClosed, ErrTransportFailure, ErrDataChannelBufferFull, context.DeadlineExceeded) {
			if errors.Is(err, datachannel.ErrDataDroppedBySlowReader) {
				t.params.Logger.Debugw("slow data reader", "error", err)
			} else {
				t.params.Logger.Warnw("send data packet error", err)
			}
		}
		if utils.ErrorIsOneOf(err, sctp.ErrStreamClosed, io.ErrClosedPipe) {
			t.params.SubscriberHandler.OnDataSendError(err)
		}
	} else {
		t.params.DataChannelStats.AddBytes(uint64(len(encoded)), true)
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

func (t *TransportManager) GetUnmatchMediaForOffer(offer webrtc.SessionDescription, mediaType string) (parsed *sdp.SessionDescription, unmatched []*sdp.MediaDescription, err error) {
	// prefer codec from offer for clients that don't support setCodecPreferences
	parsed, err = offer.Unmarshal()
	if err != nil {
		t.params.Logger.Errorw("failed to parse offer for codec preference", err)
		return
	}

	var lastMatchedMid string
	lastAnswer := t.lastPublisherAnswer.Load()
	if lastAnswer != nil {
		answer := lastAnswer.(webrtc.SessionDescription)
		parsedAnswer, err1 := answer.Unmarshal()
		if err1 != nil {
			// should not happen
			t.params.Logger.Errorw("failed to parse last answer", err)
			return
		}

		for i := len(parsedAnswer.MediaDescriptions) - 1; i >= 0; i-- {
			media := parsedAnswer.MediaDescriptions[i]
			if media.MediaName.Media == mediaType {
				lastMatchedMid, _ = media.Attribute(sdp.AttrKeyMID)
				break
			}
		}
	}

	for i := len(parsed.MediaDescriptions) - 1; i >= 0; i-- {
		media := parsed.MediaDescriptions[i]
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

func (t *TransportManager) LastPublisherOffer() webrtc.SessionDescription {
	if sd := t.lastPublisherOffer.Load(); sd != nil {
		return sd.(webrtc.SessionDescription)
	}
	return webrtc.SessionDescription{}
}

func (t *TransportManager) HandleOffer(offer webrtc.SessionDescription, shouldPend bool) error {
	t.lock.Lock()
	if shouldPend {
		t.pendingOfferPublisher = &offer
		t.lock.Unlock()
		return nil
	}
	t.lock.Unlock()
	t.lastPublisherOffer.Store(offer)

	return t.publisher.HandleRemoteDescription(offer)
}

func (t *TransportManager) GetAnswer() (webrtc.SessionDescription, error) {
	answer, err := t.publisher.GetAnswer()
	if err == nil {
		t.lastPublisherAnswer.Store(answer)
	}
	return answer, err
}

func (t *TransportManager) ProcessPendingPublisherOffer() {
	t.lock.Lock()
	pendingOffer := t.pendingOfferPublisher
	t.pendingOfferPublisher = nil
	t.lock.Unlock()

	if pendingOffer != nil {
		t.HandleOffer(*pendingOffer, false)
	}
}

func (t *TransportManager) HandleAnswer(answer webrtc.SessionDescription) {
	t.subscriber.HandleRemoteDescription(answer)
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
	t.subscriber.Negotiate(force)
}

func (t *TransportManager) HandleClientReconnect(reason livekit.ReconnectReason) {
	var (
		isShort              bool
		duration             time.Duration
		resetShortConnection bool
	)
	switch reason {
	case livekit.ReconnectReason_RR_PUBLISHER_FAILED:
		resetShortConnection = true
		isShort, duration = t.publisher.IsShortConnection(time.Now())

	case livekit.ReconnectReason_RR_SUBSCRIBER_FAILED:
		resetShortConnection = true
		isShort, duration = t.subscriber.IsShortConnection(time.Now())
	}

	if isShort {
		t.lock.Lock()
		t.resetTransportConfigureLocked(false)
		t.lock.Unlock()
		t.params.Logger.Infow("short connection by client ice restart", "duration", duration, "reason", reason)
		t.handleConnectionFailed(isShort)
	}

	if resetShortConnection {
		t.publisher.ResetShortConnOnICERestart()
		t.subscriber.ResetShortConnOnICERestart()
	}
}

func (t *TransportManager) ICERestart(iceConfig *livekit.ICEConfig) error {
	t.SetICEConfig(iceConfig)

	return t.subscriber.ICERestart()
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

	t.publisher.SetPreferTCP(iceConfig.PreferencePublisher == livekit.ICECandidateType_ICT_TCP)
	t.subscriber.SetPreferTCP(iceConfig.PreferenceSubscriber == livekit.ICECandidateType_ICT_TCP)

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
		info := pc.GetICEConnectionInfo()
		if info.HasCandidates() {
			infos = append(infos, info)
		}
	}
	return infos
}

func (t *TransportManager) getTransport(isPrimary bool) *PCTransport {
	pcTransport := t.publisher
	if (isPrimary && t.params.SubscriberAsPrimary) || (!isPrimary && !t.params.SubscriberAsPrimary) {
		pcTransport = t.subscriber
	}

	return pcTransport
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

	//
	// Checking only `PreferenceSubscriber` field although any connection failure (PUBLISHER OR SUBSCRIBER) will
	// flow through here.
	//
	// As both transports are switched to the same type on any failure, checking just subscriber should be fine.
	//
	getNext := func(ic *livekit.ICEConfig) livekit.ICECandidateType {
		if ic.PreferenceSubscriber == livekit.ICECandidateType_ICT_NONE && t.params.ClientInfo.SupportsICETCP() && t.canUseICETCP() {
			return livekit.ICECandidateType_ICT_TCP
		} else if ic.PreferenceSubscriber != livekit.ICECandidateType_ICT_TLS && t.params.TURNSEnabled {
			return livekit.ICECandidateType_ICT_TLS
		} else {
			return livekit.ICECandidateType_ICT_NONE
		}
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

func (t *TransportManager) SetMigrateInfo(previousOffer, previousAnswer *webrtc.SessionDescription, dataChannels []*livekit.DataChannelInfo) {
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

	t.subscriber.SetPreviousSdp(previousOffer, previousAnswer)
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
				t.params.SubscriberHandler.OnFailed(true, t.subscriber.GetICEConnectionInfo())
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
	t.publisher.SetSignalingRTT(rtt)
	t.subscriber.SetSignalingRTT(rtt)

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
	t.subscriber.SetAllowPauseOfStreamAllocator(allowPause)
}

func (t *TransportManager) SetSubscriberChannelCapacity(channelCapacity int64) {
	t.subscriber.SetChannelCapacityOfStreamAllocator(channelCapacity)
}

func (t *TransportManager) hasRecentSignalLocked() bool {
	return time.Since(t.lastSignalAt) < PingTimeoutSeconds*time.Second
}
