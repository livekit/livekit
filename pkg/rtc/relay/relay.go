package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/livekit/protocol/logger"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
)

const (
	signalerLabel         = "ion_sfu_relay_signaler"
	signalerAddTrackEvent = "ion_relay_add_rack"
)

var (
	ErrRelayNotReady    = errors.New("relay Peer is not ready")
	ErrRelaySignalError = errors.New("relay Peer signal state error")
)

type RelayConfig struct {
	SettingEngine webrtc.SettingEngine
	ICEServers    []webrtc.ICEServer
	BufferFactory *buffer.Factory
}

type offerAnswerSignal struct {
	ICECandidates    []webrtc.ICECandidate   `json:"iceCandidates,omitempty"`
	ICEParameters    webrtc.ICEParameters    `json:"iceParameters,omitempty"`
	DTLSParameters   webrtc.DTLSParameters   `json:"dtlsParameters,omitempty"`
	SCTPCapabilities webrtc.SCTPCapabilities `json:"sctpCapabilities,omitempty"`
}

type addTrackSignal struct {
	StreamID        string                     `json:"streamId"`
	TrackID         string                     `json:"trackId"`
	Mid             string                     `json:"mid"`
	Encodings       webrtc.RTPCodingParameters `json:"encodings,omitempty"`
	CodecParameters webrtc.RTPCodecParameters  `json:"codecParameters,omitempty"`
	Meta            string                     `json:"meta,omitempty"`
}

type dcMessage struct {
	ID      uint64 `json:"id"`
	IsReply bool   `json:"reply"`
	Event   string `json:"event"`
	Payload []byte `json:"payload"`
}

type TrackParameters interface {
	ID() string
	StreamID() string
	Kind() webrtc.RTPCodecType
	Codec() webrtc.RTPCodecParameters
	PayloadType() webrtc.PayloadType
	RID() string
}

type Relay struct {
	mu   sync.Mutex
	rmu  sync.Mutex
	rand *rand.Rand

	bufferFactory *buffer.Factory

	me          *webrtc.MediaEngine
	api         *webrtc.API
	ice         *webrtc.ICETransport
	gatherer    *webrtc.ICEGatherer
	role        webrtc.ICERole
	dtls        *webrtc.DTLSTransport
	sctp        *webrtc.SCTPTransport
	signalingDC *webrtc.DataChannel
	dcIndex     uint16

	senders     []*webrtc.RTPSender
	receivers   []*webrtc.RTPReceiver
	localTracks []webrtc.TrackLocal

	pendingRequests map[uint64]chan []byte

	ready  bool
	logger logger.Logger

	onReady                 atomic.Value // func()
	onDataChannel           atomic.Value // func(channel *webrtc.DataChannel)
	onTrack                 atomic.Value // func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver, meta *TrackMeta)
	onConnectionStateChange atomic.Value // func(state webrtc.ICETransportState)
}

func NewRelay(logger logger.Logger, conf *RelayConfig) (*Relay, error) {
	me := webrtc.MediaEngine{}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(&me), webrtc.WithSettingEngine(conf.SettingEngine))
	gatherer, err := api.NewICEGatherer(webrtc.ICEGatherOptions{ICEServers: conf.ICEServers})
	if err != nil {
		return nil, err
	}
	ice := api.NewICETransport(gatherer)
	dtls, err := api.NewDTLSTransport(ice, nil)
	sctp := api.NewSCTPTransport(dtls)
	if err != nil {
		return nil, err
	}

	r := &Relay{
		bufferFactory:   conf.BufferFactory,
		rand:            rand.New(rand.NewSource(time.Now().UnixNano())),
		me:              &me,
		api:             api,
		ice:             ice,
		gatherer:        gatherer,
		dtls:            dtls,
		sctp:            sctp,
		pendingRequests: map[uint64]chan []byte{},
		logger:          logger,
	}

	sctp.OnDataChannel(func(channel *webrtc.DataChannel) {
		logger.Infow("OnDataChannel", "channelLabel", channel.Label())

		if channel.Label() == signalerLabel {
			r.signalingDC = channel
			channel.OnMessage(r.onDataChannelMessage)
			channel.OnOpen(func() {
				if f := r.onReady.Load(); f != nil {
					f.(func())()
				}
			})
			return
		}

		if f := r.onDataChannel.Load(); f != nil {
			f.(func(dataChannel *webrtc.DataChannel))(channel)
		}
	})

	ice.OnConnectionStateChange(func(state webrtc.ICETransportState) {
		if f := r.onConnectionStateChange.Load(); f != nil {
			f.(func(webrtc.ICETransportState))(state)
		}
	})

	return r, nil
}

func (r *Relay) GetBufferFactory() *buffer.Factory {
	return r.bufferFactory
}

func (r *Relay) IsReady() bool {
	return r.ready
}

func (r *Relay) Offer(signalFn func(signal []byte) ([]byte, error)) error {
	if r.gatherer.State() == webrtc.ICEGathererStateNew {
		gatherFinished := make(chan struct{})
		r.gatherer.OnLocalCandidate(func(i *webrtc.ICECandidate) {
			if i == nil {
				close(gatherFinished)
			}
		})
		// Gather candidates
		if err := r.gatherer.Gather(); err != nil {
			return err
		}
		<-gatherFinished
	} else if r.gatherer.State() != webrtc.ICEGathererStateComplete {
		return ErrRelaySignalError
	}

	ls := &offerAnswerSignal{}

	var err error

	if ls.ICECandidates, err = r.gatherer.GetLocalCandidates(); err != nil {
		return err
	}
	if ls.ICEParameters, err = r.gatherer.GetLocalParameters(); err != nil {
		return err
	}
	if ls.DTLSParameters, err = r.dtls.GetLocalParameters(); err != nil {
		return err
	}

	ls.SCTPCapabilities = r.sctp.GetCapabilities()

	r.role = webrtc.ICERoleControlling
	data, err := json.Marshal(ls)

	remoteSignal, err := signalFn(data)
	if err != nil {
		return err
	}

	rs := &offerAnswerSignal{}

	if err = json.Unmarshal(remoteSignal, rs); err != nil {
		return err
	}

	if err = r.start(rs); err != nil {
		return err
	}

	if r.signalingDC, err = r.createDataChannel(signalerLabel); err != nil {
		return err
	}

	r.signalingDC.OnMessage(r.onDataChannelMessage)

	r.signalingDC.OnOpen(func() {
		if f := r.onReady.Load(); f != nil {
			f.(func())()
		}
	})

	return nil
}

func (r *Relay) Answer(request []byte) ([]byte, error) {
	if r.gatherer.State() == webrtc.ICEGathererStateNew {
		gatherFinished := make(chan struct{})
		r.gatherer.OnLocalCandidate(func(i *webrtc.ICECandidate) {
			if i == nil {
				close(gatherFinished)
			}
		})
		// Gather candidates
		if err := r.gatherer.Gather(); err != nil {
			return nil, err
		}
		<-gatherFinished
	} else if r.gatherer.State() != webrtc.ICEGathererStateComplete {
		return nil, ErrRelaySignalError
	}

	ls := &offerAnswerSignal{}

	var err error

	if ls.ICECandidates, err = r.gatherer.GetLocalCandidates(); err != nil {
		return nil, err
	}
	if ls.ICEParameters, err = r.gatherer.GetLocalParameters(); err != nil {
		return nil, err
	}
	if ls.DTLSParameters, err = r.dtls.GetLocalParameters(); err != nil {
		return nil, err
	}

	ls.SCTPCapabilities = r.sctp.GetCapabilities()

	r.role = webrtc.ICERoleControlled

	rs := &offerAnswerSignal{}
	if err = json.Unmarshal(request, rs); err != nil {
		return nil, err
	}

	go func() {
		if err = r.start(rs); err != nil {
			r.logger.Errorw("Error starting relay", err)
		}
	}()

	return json.Marshal(ls)
}

func (r *Relay) start(remoteSignal *offerAnswerSignal) error {
	if err := r.ice.SetRemoteCandidates(remoteSignal.ICECandidates); err != nil {
		return err
	}

	if err := r.ice.Start(r.gatherer, remoteSignal.ICEParameters, &r.role); err != nil {
		return err
	}

	if err := r.dtls.Start(remoteSignal.DTLSParameters); err != nil {
		return err
	}

	if err := r.sctp.Start(remoteSignal.SCTPCapabilities); err != nil {
		return err
	}
	r.ready = true
	return nil
}

func (r *Relay) WriteRTCP(pkts []rtcp.Packet) error {
	for _, pkt := range pkts {
		if pli, ok := pkt.(*rtcp.PictureLossIndication); ok {
			fmt.Printf("PictureLossIndication(SenderSSRC=%v, MediaSSRC=%v)\n", pli.SenderSSRC, pli.MediaSSRC)
		}
	}
	_, err := r.dtls.WriteRTCP(pkts)
	return err
}

func (r *Relay) AddTrack(rtpParameters webrtc.RTPParameters, trackParameters TrackParameters, localTrack webrtc.TrackLocal, mid string, trackMeta string) (*webrtc.RTPSender, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	codec := trackParameters.Codec()
	sdr, err := r.api.NewRTPSender(localTrack, r.dtls)
	if err != nil {
		return nil, err
	}
	if err = r.me.RegisterCodec(codec, trackParameters.Kind()); err != nil {
		return nil, err
	}

	s := &addTrackSignal{
		StreamID:        trackParameters.StreamID(),
		TrackID:         trackParameters.ID(),
		Mid:             mid,
		CodecParameters: trackParameters.Codec(),
		Encodings: webrtc.RTPCodingParameters{
			RID:         trackParameters.RID(),
			SSRC:        sdr.GetParameters().Encodings[0].SSRC,
			PayloadType: trackParameters.PayloadType(),
		},
		Meta: trackMeta,
	}
	pld, err := json.Marshal(&s)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()
	if _, err = r.request(ctx, signalerAddTrackEvent, pld); err != nil {
		return nil, err
	}

	if err = sdr.Send(webrtc.RTPSendParameters{
		RTPParameters: rtpParameters,
		Encodings: []webrtc.RTPEncodingParameters{
			{
				webrtc.RTPCodingParameters{
					SSRC:        s.Encodings.SSRC,
					PayloadType: s.Encodings.PayloadType,
				},
			},
		},
	}); err != nil {
		r.logger.Errorw("Send RTPSender failed", err)
	}

	r.localTracks = append(r.localTracks, localTrack)
	r.senders = append(r.senders, sdr)

	return sdr, nil
}

func (r *Relay) request(ctx context.Context, event string, data []byte) ([]byte, error) {
	req := dcMessage{
		ID:      r.rand.Uint64(),
		Event:   event,
		Payload: data,
	}

	msg, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	if err = r.signalingDC.Send(msg); err != nil {
		return nil, err
	}
	r.logger.Infow("data channel request sent")

	resp := make(chan []byte, 1)

	r.rmu.Lock()
	r.pendingRequests[req.ID] = resp
	r.rmu.Unlock()

	defer func() {
		r.rmu.Lock()
		delete(r.pendingRequests, req.ID)
		r.rmu.Unlock()
	}()

	select {
	case r := <-resp:
		return r, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *Relay) onDataChannelMessage(msg webrtc.DataChannelMessage) {
	r.logger.Infow("onDataChannelMessage")

	mr := &dcMessage{}
	if err := json.Unmarshal(msg.Data, mr); err != nil {
		r.logger.Errorw("Error marshaling remote message", err)
		return
	}

	if mr.Event == signalerAddTrackEvent && !mr.IsReply {
		r.logger.Infow("onDataChannelMessage ")

		r.mu.Lock()
		defer r.mu.Unlock()

		s := &addTrackSignal{}
		if err := json.Unmarshal(mr.Payload, s); err != nil {
			r.logger.Errorw("Error marshaling remote message", err)
			return
		}
		if err := r.handleAddTrack(s); err != nil {
			r.logger.Errorw("Error receiving remote track", err)
			return
		}
		if err := r.reply(mr.ID, mr.Event, nil); err != nil {
			r.logger.Errorw("Error replying message", err)
			return
		}

		return
	}

	if mr.IsReply {
		r.rmu.Lock()
		if c, ok := r.pendingRequests[mr.ID]; ok {
			c <- mr.Payload
			delete(r.pendingRequests, mr.ID)
		}
		r.rmu.Unlock()
		return
	}
}

func (r *Relay) handleAddTrack(s *addTrackSignal) error {
	var k webrtc.RTPCodecType
	switch {
	case strings.HasPrefix(s.CodecParameters.MimeType, "audio/"):
		k = webrtc.RTPCodecTypeAudio
	case strings.HasPrefix(s.CodecParameters.MimeType, "video/"):
		k = webrtc.RTPCodecTypeVideo
	default:
		k = webrtc.RTPCodecType(0)
	}
	if err := r.me.RegisterCodec(s.CodecParameters, k); err != nil {
		return err
	}

	recv, err := r.api.NewRTPReceiver(k, r.dtls)
	if err != nil {
		return err
	}

	if err = recv.Receive(webrtc.RTPReceiveParameters{Encodings: []webrtc.RTPDecodingParameters{
		{
			webrtc.RTPCodingParameters{
				// RID:         s.Encodings.RID,
				SSRC:        s.Encodings.SSRC,
				PayloadType: s.Encodings.PayloadType,
			},
		},
	}}); err != nil {
		return err
	} else {
		r.logger.Infow("RTPReceiver.Receive", "SSRC", s.Encodings.SSRC, "RID", s.Encodings.RID)
	}

	recv.SetRTPParameters(webrtc.RTPParameters{
		HeaderExtensions: nil,
		Codecs:           []webrtc.RTPCodecParameters{s.CodecParameters},
	})

	track := recv.Track()

	if f := r.onTrack.Load(); f != nil {
		f.(func(*webrtc.TrackRemote, *webrtc.RTPReceiver, string, string, string, string, string))(track, recv, s.Mid, s.TrackID, s.StreamID, s.Encodings.RID, s.Meta)
	}

	r.receivers = append(r.receivers, recv)

	return nil
}

func (r *Relay) reply(id uint64, event string, payload []byte) error {
	req := dcMessage{
		ID:      id,
		Event:   event,
		Payload: payload,
		IsReply: true,
	}

	msg, err := json.Marshal(req)
	if err != nil {
		return err
	}

	if err = r.signalingDC.Send(msg); err != nil {
		r.logger.Errorw("data channel reply sent", err)
		return err
	}
	r.logger.Infow("data channel reply sent")
	return nil
}

func (r *Relay) createDataChannel(label string) (*webrtc.DataChannel, error) {
	idx := r.dcIndex
	r.dcIndex++
	dcParams := &webrtc.DataChannelParameters{
		Label:   label,
		ID:      &idx,
		Ordered: true,
	}
	return r.api.NewDataChannel(r.sctp, dcParams)
}

func (r *Relay) OnReady(f func()) {
	r.onReady.Store(f)
}

func (r *Relay) OnDataChannel(f func(channel *webrtc.DataChannel)) {
	r.onDataChannel.Store(f)
}

func (r *Relay) OnTrack(f func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver, mid string, trackId string, streamId string, rid string, meta string)) {
	r.onTrack.Store(f)
}

func (r *Relay) OnConnectionStateChange(f func(state webrtc.ICETransportState)) {
	r.onConnectionStateChange.Store(f)
}
