package pc

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/livekit/protocol/logger"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
	"github.com/pkg/errors"

	"github.com/livekit/livekit-server/pkg/rtc/relay"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
)

const (
	signalerLabel = "ion_sfu_relay_signaler"
)

type eventType string

const (
	eventTypeAddTrack  eventType = "add_rack"
	eventTypeOffer     eventType = "offer"
	eventTypeMessage   eventType = "message"
	eventTypeRoomClose eventType = "room_close"
)

type relayState int32

const (
	RelayStateConnecting relayState = iota
	RelayStateOpen
	RelayStateClosing
	RelayStateClosed
)

type addTrackSignal struct {
	Encodings []webrtc.RTPEncodingParameters `json:"encodings,omitempty"`
	Rid       string                         `json:"rid,omitempty"`
	Meta      []byte                         `json:"meta,omitempty"`
}

type dcEvent struct {
	ID         uint64    `json:"id"`
	ReplyForID *uint64   `json:"replyForID"`
	Type       eventType `json:"type"`
	Payload    []byte    `json:"payload"`
}

type pendingInfoTrack struct {
	rid  string
	meta []byte
}

type pendingWebrtcTrack struct {
	trackRemote *webrtc.TrackRemote
	rtpReceiver *webrtc.RTPReceiver
	mid         string
}

type sessionDescriptionWithIceCandidates struct {
	SessionDescription webrtc.SessionDescription `json:"sessionDescription"`
	IceCandidates      []webrtc.ICECandidateInit `json:"iceCandidates"`
}

var opusCodec = webrtc.RTPCodecParameters{
	RTPCodecCapability: webrtc.RTPCodecCapability{
		MimeType:    webrtc.MimeTypeOpus,
		ClockRate:   48000,
		Channels:    2,
		SDPFmtpLine: "minptime=10;useinbandfec=1",
	},
	PayloadType: 111,
}

var redCodec = webrtc.RTPCodecParameters{
	RTPCodecCapability: webrtc.RTPCodecCapability{
		MimeType:    sfu.MimeTypeAudioRed,
		ClockRate:   48000,
		Channels:    2,
		SDPFmtpLine: "111/111",
	},
	PayloadType: 63,
}

var vp8Codec = webrtc.RTPCodecParameters{
	RTPCodecCapability: webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypeVP8,
		ClockRate: 90000,
		RTCPFeedback: []webrtc.RTCPFeedback{
			{Type: "nack", Parameter: ""},
			{Type: "nack", Parameter: "pli"},
		},
	},
	PayloadType: 96,
}

var vp8RtxCodec = webrtc.RTPCodecParameters{
	RTPCodecCapability: webrtc.RTPCodecCapability{
		MimeType:     "video/rtx",
		ClockRate:    90000,
		Channels:     0,
		SDPFmtpLine:  fmt.Sprintf("apt=%v", vp8Codec.PayloadType),
		RTCPFeedback: nil,
	},
	PayloadType: 97,
}

type PcRelay struct {
	id             string
	side           string
	pc             *webrtc.PeerConnection
	rand           *rand.Rand
	bufferFactory  *buffer.Factory
	signalingDC    *webrtc.DataChannel
	pendingReplies sync.Map

	pendingInfoTracks   map[webrtc.SSRC]pendingInfoTrack
	pendingWebrtcTracks map[webrtc.SSRC]pendingWebrtcTrack
	pendingTracksMu     sync.Mutex

	onReady     atomic.Value // func()
	onTrack     atomic.Value // func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver, meta *TrackMeta)
	onMessage   atomic.Value // func(message []byte)
	onICEChange atomic.Value // func(webrtc.ICEConnectionState)

	state     atomic.Int32 // relayState
	closedCh  chan struct{}
	closeOnce sync.Once

	offerMu sync.Mutex

	isReconnecting atomic.Bool

	conf *relay.RelayConfig

	logger logger.Logger
}

func NewRelay(logger logger.Logger, conf *relay.RelayConfig) (*PcRelay, error) {
	r := &PcRelay{
		id:            conf.ID,
		side:          conf.Side,
		bufferFactory: conf.BufferFactory,
		logger:        logger,
		rand:          rand.New(rand.NewSource(time.Now().UnixNano())),

		pendingInfoTracks:   map[webrtc.SSRC]pendingInfoTrack{},
		pendingWebrtcTracks: map[webrtc.SSRC]pendingWebrtcTrack{},
		closedCh:            make(chan struct{}),

		conf: conf,
	}

	pc, pcErr := r.createPeerConnection(conf)
	if pcErr != nil {
		return nil, pcErr
	}
	r.pc = pc

	r.state.Store(int32(RelayStateConnecting))

	return r, nil
}

func (r *PcRelay) createPeerConnection(conf *relay.RelayConfig) (*webrtc.PeerConnection, error) {
	conf.SettingEngine.BufferFactory = conf.BufferFactory.GetOrNew

	me := &webrtc.MediaEngine{}
	if err := me.RegisterCodec(opusCodec, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, fmt.Errorf("RegisterCodec error: %w", err)
	}
	if err := me.RegisterCodec(redCodec, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, fmt.Errorf("RegisterCodec error: %w", err)
	}
	if err := me.RegisterCodec(vp8Codec, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, fmt.Errorf("RegisterCodec error: %w", err)
	}
	if err := me.RegisterCodec(vp8RtxCodec, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, fmt.Errorf("RegisterCodec error: %w", err)
	}
	if conf.RelayUdpPort != 0 {
		if conf.Side == "in" {
			conf.SettingEngine.SetICEUDPMux(conf.RelayUDPMux)
			conf.SettingEngine.SetAnsweringDTLSRole(webrtc.DTLSRoleServer)
		} else {
			conf.SettingEngine.SetAnsweringDTLSRole(webrtc.DTLSRoleClient)
		}
		logger.Infow("Configured relay UDP port", "port", conf.RelayUdpPort, "side", conf.Side, "relayID", conf.ID)
	}

	pc, err := webrtc.
		NewAPI(webrtc.WithSettingEngine(conf.SettingEngine), webrtc.WithMediaEngine(me)).
		NewPeerConnection(webrtc.Configuration{
			ICEServers: conf.ICEServers,
		})
	if err != nil {
		return nil, err
	}

	pc.OnTrack(r.onPeerConnectionTrack)

	pc.OnDataChannel(func(channel *webrtc.DataChannel) {
		logger.Debugw("Data channel created", "channelLabel", channel.Label(), "relayID", r.id, "side", r.side)

		if channel.Label() == signalerLabel {
			r.signalingDC = channel
			channel.OnMessage(r.onSignalingDataChannelMessage)
			channel.OnOpen(func() {
				r.logger.Infow("Signaling data channel opened", "relayID", r.id, "side", r.side)
				r.state.Store(int32(RelayStateOpen))
				if f := r.onReady.Load(); f != nil {
					f.(func())()
				}
			})
			return
		}
	})

	return pc, nil
}

func (r *PcRelay) resignal() {
	r.logger.Debugw("Resignaling connection", "relayID", r.id, "side", r.side)

	r.offerMu.Lock()
	defer r.offerMu.Unlock()

	offer, offerErr := r.pc.CreateOffer(nil)
	if offerErr != nil {
		r.logger.Errorw("Failed to create offer", offerErr, "relayID", r.id, "side", r.side)
		return
	}

	if err := r.pc.SetLocalDescription(offer); err != nil {
		r.logger.Errorw("Failed to set local description", err, "relayID", r.id, "side", r.side)
		return
	}

	offerData, marshalErr := json.Marshal(offer)
	if marshalErr != nil {
		r.logger.Errorw("Failed to marshal offer", marshalErr, "relayID", r.id, "side", r.side)
		return
	}

	event := dcEvent{
		ID:      r.rand.Uint64(),
		Type:    eventTypeOffer,
		Payload: offerData,
	}

	replyCh, sendErr := r.send(event, true)
	if sendErr != nil {
		r.logger.Errorw("Failed to send offer", sendErr, "relayID", r.id, "side", r.side)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	select {
	case answerData := <-replyCh:
		answer := webrtc.SessionDescription{}
		if err := json.Unmarshal(answerData, &answer); err != nil {
			r.logger.Errorw("Failed to unmarshal answer", err, "relayID", r.id, "side", r.side)
			return
		}

		if err := r.pc.SetRemoteDescription(answer); err != nil {
			r.logger.Errorw("Failed to set remote description", err, "relayID", r.id, "side", r.side)
			return
		}
	case <-ctx.Done():
		r.logger.Errorw("Timeout waiting for answer", ctx.Err(), "relayID", r.id, "side", r.side, "timeout", "5s")
	}
}

func (r *PcRelay) Offer(signalFn func(offerData []byte) ([]byte, error)) error {
	ordered := true
	var dcErr error
	r.signalingDC, dcErr = r.pc.CreateDataChannel(signalerLabel, &webrtc.DataChannelInit{
		Ordered: &ordered,
	})
	if dcErr != nil {
		return errors.Wrap(dcErr, "CreateDataChannel error")
	}

	r.signalingDC.OnMessage(r.onSignalingDataChannelMessage)
	r.signalingDC.OnOpen(func() {
		if f := r.onReady.Load(); f != nil {
			f.(func())()
		}
	})

	offer, offerErr := r.pc.CreateOffer(nil)
	if offerErr != nil {
		return errors.Wrap(offerErr, "CreateOffer error")
	}

	doneCh := make(chan struct{})
	var iceCandidates []webrtc.ICECandidateInit

	r.pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			close(doneCh)
		} else {
			iceCandidates = append(iceCandidates, candidate.ToJSON())
		}
	})

	if err := r.pc.SetLocalDescription(offer); err != nil {
		return errors.Wrap(err, "SetLocalDescription error")
	}

	select {
	case <-doneCh:
	case <-time.After(5 * time.Second):
		r.logger.Warnw("Timeout waiting for ICE candidates", nil, "candidatesCount", len(iceCandidates), "relayID", r.id, "side", r.side)
	}

	offerWithIceCandidates := sessionDescriptionWithIceCandidates{offer, iceCandidates}
	offerData, marshalErr := json.Marshal(offerWithIceCandidates)
	if marshalErr != nil {
		return errors.Wrap(marshalErr, "json marshal error")
	}
	r.logger.Debugw("Offer created", "value", offerWithIceCandidates)

	answerData, signalErr := signalFn(offerData)
	if signalErr != nil {
		return errors.Wrap(signalErr, "signalFn error")
	}

	answerWithIceCandidates := sessionDescriptionWithIceCandidates{}
	if err := json.Unmarshal(answerData, &answerWithIceCandidates); err != nil {
		return errors.Wrap(err, "json unmarshal error")
	}

	r.logger.Debugw("Answer received", "value", answerWithIceCandidates)

	if err := r.pc.SetRemoteDescription(answerWithIceCandidates.SessionDescription); err != nil {
		return errors.Wrap(err, "SetRemoteDescription error")
	}

	for _, candidate := range answerWithIceCandidates.IceCandidates {
		if err := r.pc.AddICECandidate(candidate); err != nil {
			return errors.Wrap(err, "AddICECandidate error")
		}
	}

	return nil
}

func (r *PcRelay) Answer(offerData []byte) ([]byte, error) {
	offerWithIceCandidates := sessionDescriptionWithIceCandidates{}
	if err := json.Unmarshal(offerData, &offerWithIceCandidates); err != nil {
		return nil, errors.Wrap(err, "json unmarshal error")
	}

	r.logger.Debugw("Received offer for answer", "value", offerWithIceCandidates)

	doneCh := make(chan struct{})
	var iceCandidates []webrtc.ICECandidateInit

	r.pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			close(doneCh)
		} else {
			iceCandidates = append(iceCandidates, candidate.ToJSON())
		}
	})

	if err := r.pc.SetRemoteDescription(offerWithIceCandidates.SessionDescription); err != nil {
		return nil, errors.Wrap(err, "SetRemoteDescription error")
	}

	answer, answerErr := r.pc.CreateAnswer(nil)
	if answerErr != nil {
		return nil, errors.Wrap(answerErr, "CreateAnswer error")
	}

	if err := r.pc.SetLocalDescription(answer); err != nil {
		return nil, errors.Wrap(err, "SetLocalDescription error")
	}

	for _, candidate := range offerWithIceCandidates.IceCandidates {
		if err := r.pc.AddICECandidate(candidate); err != nil {
			return nil, errors.Wrap(err, "AddICECandidate error")
		}
	}

	select {
	case <-doneCh:
	case <-time.After(5 * time.Second):
		r.logger.Warnw("Timeout waiting for ICE candidates", nil, "candidatesCount", len(iceCandidates), "relayID", r.id, "side", r.side)
	}

	answerWithIceCandidates := sessionDescriptionWithIceCandidates{answer, iceCandidates}

	r.logger.Debugw("Created answer", "value", answerWithIceCandidates)

	data, marshalErr := json.Marshal(answerWithIceCandidates)
	if marshalErr != nil {
		return nil, errors.Wrap(marshalErr, "json marshal error")
	}

	return data, nil
}

func (r *PcRelay) ID() string {
	return r.id
}

func (r *PcRelay) GetBufferFactory() *buffer.Factory {
	return r.bufferFactory
}

func (r *PcRelay) WriteRTCP(pkts []rtcp.Packet) error {
	return r.pc.WriteRTCP(pkts)
}

func (r *PcRelay) AddTrack(ctx context.Context, track webrtc.TrackLocal, trackRid string, trackMeta []byte) (*webrtc.RTPSender, error) {
	if rtpSender, err := r.pc.AddTrack(track); err != nil {
		return nil, err
	} else {
		go r.resignal()

		signal := &addTrackSignal{
			Encodings: rtpSender.GetParameters().Encodings,
			Rid:       trackRid,
			Meta:      trackMeta,
		}
		signalPayload, marshalErr := json.Marshal(signal)
		if marshalErr != nil {
			return nil, fmt.Errorf("marshal error: %w", marshalErr)
		}

		event := dcEvent{
			ID:      r.rand.Uint64(),
			Type:    eventTypeAddTrack,
			Payload: signalPayload,
		}

		replyCh, sendErr := r.send(event, true)
		if sendErr != nil {
			return nil, fmt.Errorf("send error: %w", sendErr)
		}

		select {
		case <-replyCh:
			r.logger.Debugw("Add track reply received", "relayID", r.id, "side", r.side)
		case <-ctx.Done():
			return nil, fmt.Errorf("add track context err: %w", ctx.Err())
		}

		return rtpSender, nil
	}
}

func (r *PcRelay) RemoveTrack(sender *webrtc.RTPSender) error {
	if err := r.pc.RemoveTrack(sender); err != nil {
		return err
	}

	go r.resignal()

	return nil
}

func (r *PcRelay) OnReady(f func()) {
	r.onReady.Store(f)
}

func (r *PcRelay) OnTrack(f func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver, mid string, rid string, meta []byte)) {
	r.onTrack.Store(f)
}

func (r *PcRelay) OnConnectionStateChange(f func(state webrtc.ICEConnectionState)) {
	r.onICEChange.Store(f)
	r.pc.OnICEConnectionStateChange(f)
}

func (r *PcRelay) SendMessage(payload []byte) error {
	event := dcEvent{
		ID:      r.rand.Uint64(),
		Type:    eventTypeMessage,
		Payload: payload,
	}
	_, err := r.send(event, false)
	return err
}

func (r *PcRelay) SendRoomCloseMessage() error {
	if r.signalingDC == nil || r.signalingDC.ReadyState() != webrtc.DataChannelStateOpen {
		return nil
	}

	event := dcEvent{
		ID:   r.rand.Uint64(),
		Type: eventTypeRoomClose,
	}
	_, err := r.send(event, false)
	return err
}

func (r *PcRelay) SendReplyMessage(replyForID uint64, payload []byte) error {
	event := dcEvent{
		ID:         r.rand.Uint64(),
		ReplyForID: &replyForID,
		Type:       eventTypeMessage,
		Payload:    payload,
	}
	_, err := r.send(event, false)
	return err
}

func (r *PcRelay) SendMessageAndExpectReply(payload []byte) (<-chan []byte, error) {
	event := dcEvent{
		ID:      r.rand.Uint64(),
		Type:    eventTypeMessage,
		Payload: payload,
	}
	return r.send(event, true)
}

func (r *PcRelay) DebugInfo() map[string]interface{} {
	stats := map[string]interface{}{
		"id":                 r.id,
		"side":               r.side,
		"iceConnectionState": r.pc.ICEConnectionState().String(),
		"connectionState":    r.pc.ConnectionState().String(),
		"signalingState":     r.pc.SignalingState().String(),
		"hasPendingTracks":   len(r.pendingInfoTracks) > 0 || len(r.pendingWebrtcTracks) > 0,
	}
	return stats
}

func (r *PcRelay) send(event dcEvent, replyExpected bool) (<-chan []byte, error) {
	data, marshalErr := json.Marshal(event)
	if marshalErr != nil {
		r.logger.Errorw("Failed to marshal data channel event", marshalErr, "eventType", event.Type, "relayID", r.id, "side", r.side)
		return nil, fmt.Errorf("can not marshal DC event: %w", marshalErr)
	}
	var reply chan []byte
	if replyExpected {
		reply = make(chan []byte, 1)
		r.pendingReplies.Store(event.ID, reply)
	}
	if err := r.signalingDC.Send(data); err != nil {
		r.pendingReplies.Delete(event.ID)
		r.logger.Errorw("Failed to send data channel event", err, "eventType", event.Type, "relayID", r.id, "side", r.side, "replyExpected", replyExpected)
		return nil, fmt.Errorf("can not send DC event: %w", err)
	}
	return reply, nil
}

func (r *PcRelay) onPeerConnectionTrack(trackRemote *webrtc.TrackRemote, rtpReceiver *webrtc.RTPReceiver) {
	r.pendingTracksMu.Lock()
	defer r.pendingTracksMu.Unlock()

	if infoTrack, ok := r.pendingInfoTracks[trackRemote.SSRC()]; ok {
		delete(r.pendingInfoTracks, trackRemote.SSRC())
		if f := r.onTrack.Load(); f != nil {
			f.(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver, mid string, rid string, meta []byte))(
				trackRemote,
				rtpReceiver,
				r.getMid(rtpReceiver),
				infoTrack.rid,
				infoTrack.meta,
			)
		}
	} else {
		r.pendingWebrtcTracks[trackRemote.SSRC()] = pendingWebrtcTrack{trackRemote, rtpReceiver, r.getMid(rtpReceiver)}
	}
}

func (r *PcRelay) onAddTrackSignal(signal *addTrackSignal) {
	r.pendingTracksMu.Lock()
	defer r.pendingTracksMu.Unlock()

	for _, encoding := range signal.Encodings {
		if webrtcTrack, ok := r.pendingWebrtcTracks[encoding.SSRC]; ok {
			delete(r.pendingWebrtcTracks, encoding.SSRC)
			trackRemote, rtpReceiver, mid := webrtcTrack.trackRemote, webrtcTrack.rtpReceiver, webrtcTrack.mid
			if f := r.onTrack.Load(); f != nil {
				f.(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver, mid string, rid string, meta []byte))(
					trackRemote,
					rtpReceiver,
					mid,
					signal.Rid,
					signal.Meta,
				)
			}
		} else {
			r.pendingInfoTracks[encoding.SSRC] = pendingInfoTrack{signal.Rid, signal.Meta}
		}
	}
}

func (r *PcRelay) getMid(rtpReceiver *webrtc.RTPReceiver) string {
	for _, tr := range r.pc.GetTransceivers() {
		if tr.Receiver() == rtpReceiver {
			return tr.Mid()
		}
	}

	return ""
}

func (r *PcRelay) onAddTrackOffer(offer webrtc.SessionDescription) ([]byte, error) {
	if err := r.pc.SetRemoteDescription(offer); err != nil {
		return nil, fmt.Errorf("set remote description error: %w", err)
	}

	answer, answerErr := r.pc.CreateAnswer(nil)
	if answerErr != nil {
		return nil, fmt.Errorf("create answer error: %w", answerErr)
	}

	if err := r.pc.SetLocalDescription(answer); err != nil {
		return nil, fmt.Errorf("set local description error: %w", err)
	}

	if data, err := json.Marshal(answer); err != nil {
		return nil, fmt.Errorf("marshal error: %w", err)
	} else {
		return data, nil
	}
}

func (r *PcRelay) onSignalingDataChannelMessage(msg webrtc.DataChannelMessage) {
	r.logger.Debugw("Received data channel message", "relayID", r.id, "side", r.side)

	event := &dcEvent{}
	if err := json.Unmarshal(msg.Data, event); err != nil {
		r.logger.Errorw("Failed to unmarshal remote message", err, "relayID", r.id, "side", r.side)
		return
	}

	if event.ReplyForID != nil {
		r.logger.Debugw("Reply message received", "relayID", r.id, "side", r.side, "replyForID", *event.ReplyForID)

		replyForID := *event.ReplyForID
		if replyCh, loaded := r.pendingReplies.LoadAndDelete(replyForID); loaded {
			replyCh.(chan []byte) <- event.Payload
		} else {
			r.logger.Warnw("Received reply for unknown request", nil, "replyForID", replyForID, "relayID", r.id, "side", r.side)
		}
	} else if event.Type == eventTypeAddTrack {
		r.logger.Debugw("Add track message received", "relayID", r.id, "side", r.side)

		s := &addTrackSignal{}
		if err := json.Unmarshal(event.Payload, s); err != nil {
			r.logger.Errorw("Failed to unmarshal add track signal", err, "relayID", r.id, "side", r.side)
			return
		}

		r.onAddTrackSignal(s)

		replyEvent := dcEvent{
			ID:         r.rand.Uint64(),
			ReplyForID: &event.ID,
			Type:       event.Type,
		}

		if _, err := r.send(replyEvent, false); err != nil {
			r.logger.Errorw("Failed to send add track reply", err, "relayID", r.id, "side", r.side)
			return
		}
	} else if event.Type == eventTypeOffer {
		r.logger.Debugw("Offer message received", "relayID", r.id, "side", r.side)

		sdp := webrtc.SessionDescription{}
		if err := json.Unmarshal(event.Payload, &sdp); err != nil {
			r.logger.Errorw("Failed to unmarshal offer", err, "relayID", r.id, "side", r.side)
			return
		}

		answerData, err := r.onAddTrackOffer(sdp)
		if err != nil {
			r.logger.Errorw("Failed to process offer", err, "relayID", r.id, "side", r.side)
			return
		}

		replyEvent := dcEvent{
			ID:         r.rand.Uint64(),
			ReplyForID: &event.ID,
			Type:       event.Type,
			Payload:    answerData,
		}

		if _, err := r.send(replyEvent, false); err != nil {
			r.logger.Errorw("Failed to send offer reply", err, "relayID", r.id, "side", r.side)
			return
		}
	} else if event.Type == eventTypeMessage {
		r.logger.Debugw("Custom message received", "relayID", r.id, "side", r.side)

		if f := r.onMessage.Load(); f != nil {
			f.(func(id uint64, payload []byte))(event.ID, event.Payload)
		}
	} else if event.Type == eventTypeRoomClose {
		r.logger.Debugw("Room close message received", "relayID", r.id, "side", r.side)
		r.Close()
	}
}

func (r *PcRelay) OnMessage(f func(id uint64, payload []byte)) {
	r.onMessage.Store(f)
}

func (r *PcRelay) Closed() <-chan struct{} { return r.closedCh }

func (r *PcRelay) signalClosed() {
	r.closeOnce.Do(func() { close(r.closedCh) })
}

func (r *PcRelay) Close() {
	prev := relayState(r.state.Swap(int32(RelayStateClosing)))
	if prev == RelayStateClosing || prev == RelayStateClosed {
		r.logger.Debugw("Relay already closing", "relayID", r.id, "side", r.side)
		return
	}

	go func() {
		_ = r.pc.Close()
		r.state.Store(int32(RelayStateClosed))
		r.signalClosed()
	}()
}

func (r *PcRelay) StartReconnect(inSchedule func(peerId string) error) {
	if r.side != "in" {
		return
	}

	if !r.isReconnecting.CompareAndSwap(false, true) {
		return
	}

	go func() {
		defer r.isReconnecting.Store(false)

		_ = r.pc.Close()

		r.pendingTracksMu.Lock()
		r.pendingInfoTracks = map[webrtc.SSRC]pendingInfoTrack{}
		r.pendingWebrtcTracks = map[webrtc.SSRC]pendingWebrtcTrack{}
		r.pendingTracksMu.Unlock()

		pc, err := r.createPeerConnection(r.conf)
		if err != nil {
			r.logger.Errorw("recreate PC failed", err, "relayID", r.id, "side", r.side)
			r.StartReconnect(inSchedule)
			return
		}
		r.pc = pc
		r.pc.OnICEConnectionStateChange(r.onICEChange.Load().(func(webrtc.ICEConnectionState)))
		r.state.Store(int32(RelayStateConnecting))

		if err := inSchedule(r.id); err != nil {
			r.logger.Errorw("send RECONNECT_REQUEST failed", err, "relayID", r.id, "side", r.side)
			r.StartReconnect(inSchedule)
			return
		}
	}()
}

func (r *PcRelay) RecreatePc() error {
	if r.pc != nil {
		_ = r.pc.Close()
	}

	r.pendingTracksMu.Lock()
	r.pendingInfoTracks = map[webrtc.SSRC]pendingInfoTrack{}
	r.pendingWebrtcTracks = map[webrtc.SSRC]pendingWebrtcTrack{}
	r.pendingTracksMu.Unlock()

	r.signalingDC = nil
	r.pendingReplies = sync.Map{}

	pc, err := r.createPeerConnection(r.conf)
	if err != nil {
		return err
	}
	r.pc = pc
	r.pc.OnICEConnectionStateChange(r.onICEChange.Load().(func(webrtc.ICEConnectionState)))

	r.state.Store(int32(RelayStateConnecting))
	return nil
}

func (r *PcRelay) State() relayState {
	return relayState(r.state.Load())
}
