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

	"github.com/go-logr/logr"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
)

const (
	signalerLabel        = "ion_sfu_relay_signaler"
	signalerRequestEvent = "ion_relay_request"
)

var (
	ErrRelayPeerNotReady     = errors.New("relay Peer is not ready")
	ErrRelayPeerSignalDone   = errors.New("relay Peer signal already called")
	ErrRelaySignalDCNotReady = errors.New("relay Peer data channel is not ready")
)

type signal struct {
	Encodings        *webrtc.RTPCodingParameters `json:"encodings,omitempty"`
	ICECandidates    []webrtc.ICECandidate       `json:"iceCandidates,omitempty"`
	ICEParameters    webrtc.ICEParameters        `json:"iceParameters,omitempty"`
	DTLSParameters   webrtc.DTLSParameters       `json:"dtlsParameters,omitempty"`
	SCTPCapabilities *webrtc.SCTPCapabilities    `json:"sctpCapabilities,omitempty"`
	TrackMeta        *TrackMeta                  `json:"trackInfo,omitempty"`
}

type request struct {
	ID      uint64 `json:"id"`
	IsReply bool   `json:"reply"`
	Event   string `json:"event"`
	Payload []byte `json:"payload"`
}

type TrackMeta struct {
	StreamID        string                     `json:"streamId"`
	TrackID         string                     `json:"trackId"`
	CodecParameters *webrtc.RTPCodecParameters `json:"codecParameters,omitempty"`
}

type PeerConfig struct {
	SettingEngine webrtc.SettingEngine
	ICEServers    []webrtc.ICEServer
	Logger        logr.Logger
}

type PeerMeta struct {
	PeerID    string `json:"peerId"`
	SessionID string `json:"sessionId"`
}

type Options struct {
	// RelayMiddlewareDC if set to true middleware data channels will be created and forwarded
	// to the relayed peer
	RelayMiddlewareDC bool
	// RelaySessionDC if set to true fanout data channels will be created and forwarded to the
	// relayed peer
	RelaySessionDC bool
}

type Peer struct {
	mu              sync.Mutex
	rmu             sync.Mutex
	me              *webrtc.MediaEngine
	log             logr.Logger
	api             *webrtc.API
	ice             *webrtc.ICETransport
	rand            *rand.Rand
	meta            PeerMeta
	sctp            *webrtc.SCTPTransport
	dtls            *webrtc.DTLSTransport
	role            *webrtc.ICERole
	ready           bool
	senders         []*webrtc.RTPSender
	receivers       []*webrtc.RTPReceiver
	pendingRequests map[uint64]chan []byte
	localTracks     []webrtc.TrackLocal
	signalingDC     *webrtc.DataChannel
	gatherer        *webrtc.ICEGatherer
	dcIndex         uint16

	onReady       atomic.Value // func()
	onClose       atomic.Value // func()
	onRequest     atomic.Value // func(event string, message Message)
	onDataChannel atomic.Value // func(channel *webrtc.DataChannel)
	onTrack       atomic.Value // func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver, meta *TrackMeta)
}

func NewPeer(meta PeerMeta, conf *PeerConfig) (*Peer, error) {
	// Prepare ICE gathering options
	iceOptions := webrtc.ICEGatherOptions{
		ICEServers: conf.ICEServers,
	}
	me := webrtc.MediaEngine{}
	// Create an API object
	api := webrtc.NewAPI(webrtc.WithMediaEngine(&me), webrtc.WithSettingEngine(conf.SettingEngine))
	// Create the ICE gatherer
	gatherer, err := api.NewICEGatherer(iceOptions)
	if err != nil {
		return nil, err
	}
	// Construct the ICE transport
	i := api.NewICETransport(gatherer)
	// Construct the DTLS transport
	dtls, err := api.NewDTLSTransport(i, nil)
	// Construct the SCTP transport
	sctp := api.NewSCTPTransport(dtls)
	if err != nil {
		return nil, err
	}

	p := &Peer{
		me:              &me,
		api:             api,
		log:             conf.Logger,
		ice:             i,
		rand:            rand.New(rand.NewSource(time.Now().UnixNano())),
		meta:            meta,
		sctp:            sctp,
		dtls:            dtls,
		gatherer:        gatherer,
		pendingRequests: make(map[uint64]chan []byte),
	}

	sctp.OnDataChannel(func(channel *webrtc.DataChannel) {
		if channel.Label() == signalerLabel {
			p.signalingDC = channel
			channel.OnMessage(p.handleRequest)
			channel.OnOpen(func() {
				if f := p.onReady.Load(); f != nil {
					f.(func())()
				}
			})
			return
		}

		if f := p.onDataChannel.Load(); f != nil {
			f.(func(dataChannel *webrtc.DataChannel))(channel)
		}
	})

	i.OnConnectionStateChange(func(state webrtc.ICETransportState) {
		if state == webrtc.ICETransportStateFailed || state == webrtc.ICETransportStateDisconnected {
			if err = p.Close(); err != nil {
				p.log.Error(err, "Closing relayed p error")
			}
		}
	})

	return p, nil
}

func (p *Peer) ID() string {
	return p.meta.PeerID
}

// Offer is used for establish the connection of the local relay Peer
// with the remote relay Peer.
//
// If connection is successful OnReady handler will be called
func (p *Peer) Offer(signalFn func(meta PeerMeta, signal []byte) ([]byte, error)) error {
	if p.gatherer.State() != webrtc.ICEGathererStateNew {
		return ErrRelayPeerSignalDone
	}

	ls := &signal{}
	gatherFinished := make(chan struct{})
	p.gatherer.OnLocalCandidate(func(i *webrtc.ICECandidate) {
		if i == nil {
			close(gatherFinished)
		}
	})
	// Gather candidates
	if err := p.gatherer.Gather(); err != nil {
		return err
	}
	<-gatherFinished

	var err error

	if ls.ICECandidates, err = p.gatherer.GetLocalCandidates(); err != nil {
		return err
	}
	if ls.ICEParameters, err = p.gatherer.GetLocalParameters(); err != nil {
		return err
	}
	if ls.DTLSParameters, err = p.dtls.GetLocalParameters(); err != nil {
		return err
	}

	sc := p.sctp.GetCapabilities()
	ls.SCTPCapabilities = &sc

	role := webrtc.ICERoleControlling
	p.role = &role
	data, err := json.Marshal(ls)

	remoteSignal, err := signalFn(p.meta, data)
	if err != nil {
		return err
	}

	rs := &signal{}

	if err = json.Unmarshal(remoteSignal, rs); err != nil {
		return err
	}

	if err = p.start(rs); err != nil {
		return err
	}

	if p.signalingDC, err = p.createDataChannel(signalerLabel); err != nil {
		return err
	}

	p.signalingDC.OnOpen(func() {
		if f := p.onReady.Load(); f != nil {
			f.(func())()
		}
	})
	p.signalingDC.OnMessage(p.handleRequest)
	return nil
}

// OnClose sets a callback that is called when relay Peer is closed.
func (p *Peer) OnClose(fn func()) {
	p.onClose.Store(fn)
}

// Answer answers the remote Peer signal signalRequest
func (p *Peer) Answer(request []byte) ([]byte, error) {
	if p.gatherer.State() != webrtc.ICEGathererStateNew {
		return nil, ErrRelayPeerSignalDone
	}

	ls := &signal{}
	gatherFinished := make(chan struct{})
	p.gatherer.OnLocalCandidate(func(i *webrtc.ICECandidate) {
		if i == nil {
			close(gatherFinished)
		}
	})
	// Gather candidates
	if err := p.gatherer.Gather(); err != nil {
		return nil, err
	}
	<-gatherFinished

	var err error

	if ls.ICECandidates, err = p.gatherer.GetLocalCandidates(); err != nil {
		return nil, err
	}
	if ls.ICEParameters, err = p.gatherer.GetLocalParameters(); err != nil {
		return nil, err
	}
	if ls.DTLSParameters, err = p.dtls.GetLocalParameters(); err != nil {
		return nil, err
	}

	sc := p.sctp.GetCapabilities()
	ls.SCTPCapabilities = &sc

	role := webrtc.ICERoleControlled
	p.role = &role

	rs := &signal{}
	if err = json.Unmarshal(request, rs); err != nil {
		return nil, err
	}

	go func() {
		if err = p.start(rs); err != nil {
			p.log.Error(err, "Error starting relay")
		}
	}()

	return json.Marshal(ls)
}

// WriteRTCP sends a user provided RTCP packet to the connected Peer. If no Peer is connected the
// packet is discarded. It also runs any configured interceptors.
func (p *Peer) WriteRTCP(pkts []rtcp.Packet) error {
	_, err := p.dtls.WriteRTCP(pkts)
	return err
}

func (p *Peer) LocalTracks() []webrtc.TrackLocal {
	return p.localTracks
}

// OnReady calls the callback when relay Peer is ready to start sending/receiving and creating DC
func (p *Peer) OnReady(f func()) {
	p.onReady.Store(f)
}

// OnRequest calls the callback when Peer gets a request message from remote Peer
func (p *Peer) OnRequest(f func(event string, msg Message)) {
	p.onRequest.Store(f)
}

// OnDataChannel sets an event handler which is invoked when a data
// channel message arrives from a remote Peer.
func (p *Peer) OnDataChannel(f func(channel *webrtc.DataChannel)) {
	p.onDataChannel.Store(f)
}

// OnTrack sets an event handler which is called when remote track
// arrives from a remote Peer
func (p *Peer) OnTrack(f func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver, meta *TrackMeta)) {
	p.onTrack.Store(f)
}

// Close ends the relay Peer
func (p *Peer) Close() error {
	closeErrs := make([]error, 3+len(p.senders)+len(p.receivers))
	for _, sdr := range p.senders {
		closeErrs = append(closeErrs, sdr.Stop())
	}
	for _, recv := range p.receivers {
		closeErrs = append(closeErrs, recv.Stop())
	}

	closeErrs = append(closeErrs, p.sctp.Stop(), p.dtls.Stop(), p.ice.Stop())

	if f := p.onClose.Load(); f != nil {
		f.(func())()
	}

	return joinErrs(closeErrs...)
}

// CreateDataChannel creates a new DataChannel object with the given label
func (p *Peer) CreateDataChannel(label string) (*webrtc.DataChannel, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.ready {
		return nil, ErrRelayPeerNotReady
	}

	return p.createDataChannel(label)
}

func (p *Peer) createDataChannel(label string) (*webrtc.DataChannel, error) {
	idx := p.dcIndex
	p.dcIndex = +1
	dcParams := &webrtc.DataChannelParameters{
		Label:   label,
		ID:      &idx,
		Ordered: true,
	}
	return p.api.NewDataChannel(p.sctp, dcParams)
}

func (p *Peer) start(s *signal) error {
	if err := p.ice.SetRemoteCandidates(s.ICECandidates); err != nil {
		return err
	}

	if err := p.ice.Start(p.gatherer, s.ICEParameters, p.role); err != nil {
		return err
	}

	if err := p.dtls.Start(s.DTLSParameters); err != nil {
		return err
	}

	if s.SCTPCapabilities != nil {
		if err := p.sctp.Start(*s.SCTPCapabilities); err != nil {
			return err
		}
	}
	p.ready = true
	return nil
}

func (p *Peer) receive(s *signal) error {
	var k webrtc.RTPCodecType
	switch {
	case strings.HasPrefix(s.TrackMeta.CodecParameters.MimeType, "audio/"):
		k = webrtc.RTPCodecTypeAudio
	case strings.HasPrefix(s.TrackMeta.CodecParameters.MimeType, "video/"):
		k = webrtc.RTPCodecTypeVideo
	default:
		k = webrtc.RTPCodecType(0)
	}
	if err := p.me.RegisterCodec(*s.TrackMeta.CodecParameters, k); err != nil {
		return err
	}

	recv, err := p.api.NewRTPReceiver(k, p.dtls)
	if err != nil {
		return err
	}

	if err = recv.Receive(webrtc.RTPReceiveParameters{Encodings: []webrtc.RTPDecodingParameters{
		{
			webrtc.RTPCodingParameters{
				RID:         s.Encodings.RID,
				SSRC:        s.Encodings.SSRC,
				PayloadType: s.Encodings.PayloadType,
			},
		},
	}}); err != nil {
		return err
	}

	recv.SetRTPParameters(webrtc.RTPParameters{
		HeaderExtensions: nil,
		Codecs:           []webrtc.RTPCodecParameters{*s.TrackMeta.CodecParameters},
	})

	track := recv.Track()

	if f := p.onTrack.Load(); f != nil {
		f.(func(remote *webrtc.TrackRemote, receiver *webrtc.RTPReceiver, meta *TrackMeta))(track, recv, s.TrackMeta)
	}

	p.receivers = append(p.receivers, recv)

	return nil
}

// AddTrack is used to negotiate a track to the remote peer
func (p *Peer) AddTrack(receiver *webrtc.RTPReceiver, remoteTrack *webrtc.TrackRemote,
	localTrack webrtc.TrackLocal) (*webrtc.RTPSender, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	codec := remoteTrack.Codec()
	sdr, err := p.api.NewRTPSender(localTrack, p.dtls)
	if err != nil {
		return nil, err
	}
	if err = p.me.RegisterCodec(codec, remoteTrack.Kind()); err != nil {
		return nil, err
	}

	s := &signal{}

	s.TrackMeta = &TrackMeta{
		StreamID:        remoteTrack.StreamID(),
		TrackID:         remoteTrack.ID(),
		CodecParameters: &codec,
	}

	s.Encodings = &webrtc.RTPCodingParameters{
		SSRC:        sdr.GetParameters().Encodings[0].SSRC,
		PayloadType: remoteTrack.PayloadType(),
	}
	pld, err := json.Marshal(&s)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*2)
	defer cancel()
	if _, err = p.Request(ctx, signalerRequestEvent, pld); err != nil {
		return nil, err
	}

	params := receiver.GetParameters()

	if err = sdr.Send(webrtc.RTPSendParameters{
		RTPParameters: params,
		Encodings: []webrtc.RTPEncodingParameters{
			{
				webrtc.RTPCodingParameters{
					SSRC:        s.Encodings.SSRC,
					PayloadType: s.Encodings.PayloadType,
				},
			},
		},
	}); err != nil {
		p.log.Error(err, "Send RTPSender failed")
	}

	p.localTracks = append(p.localTracks, localTrack)
	p.senders = append(p.senders, sdr)
	return sdr, nil
}

// Emit emits the data argument to remote peer.
func (p *Peer) Emit(event string, data []byte) error {
	req := request{
		ID:      p.rand.Uint64(),
		Event:   event,
		Payload: data,
	}

	msg, err := json.Marshal(req)
	if err != nil {
		return err
	}

	return p.signalingDC.Send(msg)
}

func (p *Peer) Request(ctx context.Context, event string, data []byte) ([]byte, error) {
	req := request{
		ID:      p.rand.Uint64(),
		Event:   event,
		Payload: data,
	}

	msg, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	if err = p.signalingDC.Send(msg); err != nil {
		return nil, err
	}

	resp := make(chan []byte, 1)

	p.rmu.Lock()
	p.pendingRequests[req.ID] = resp
	p.rmu.Unlock()

	defer func() {
		p.rmu.Lock()
		delete(p.pendingRequests, req.ID)
		p.rmu.Unlock()
	}()

	select {
	case r := <-resp:
		return r, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *Peer) handleRequest(msg webrtc.DataChannelMessage) {
	mr := &request{}
	if err := json.Unmarshal(msg.Data, mr); err != nil {
		p.log.Error(err, "Error marshaling remote message", "peer_id", p.meta.PeerID, "session_id", p.meta.SessionID)
		return
	}

	if mr.Event == signalerRequestEvent && !mr.IsReply {
		p.mu.Lock()
		defer p.mu.Unlock()

		r := &signal{}
		if err := json.Unmarshal(mr.Payload, r); err != nil {
			p.log.Error(err, "Error marshaling remote message", "peer_id", p.meta.PeerID, "session_id", p.meta.SessionID)
			return
		}
		if err := p.receive(r); err != nil {
			p.log.Error(err, "Error receiving remote track", "peer_id", p.meta.PeerID, "session_id", p.meta.SessionID)
			return
		}
		if err := p.reply(mr.ID, mr.Event, nil); err != nil {
			p.log.Error(err, "Error replying message", "peer_id", p.meta.PeerID, "session_id", p.meta.SessionID)
			return
		}

		return
	}

	if mr.IsReply {
		p.rmu.Lock()
		if c, ok := p.pendingRequests[mr.ID]; ok {
			c <- mr.Payload
			delete(p.pendingRequests, mr.ID)
		}
		p.rmu.Unlock()
		return
	}

	if mr.Event != signalerRequestEvent {
		if f := p.onRequest.Load(); f != nil {
			f.(func(string, Message))(mr.Event, Message{
				p:     p,
				event: mr.Event,
				id:    mr.ID,
				msg:   mr.Payload,
			})
		}
		return
	}

}

func (p *Peer) reply(id uint64, event string, payload []byte) error {
	req := request{
		ID:      id,
		Event:   event,
		Payload: payload,
		IsReply: true,
	}

	msg, err := json.Marshal(req)
	if err != nil {
		return err
	}

	if err = p.signalingDC.Send(msg); err != nil {
		return err
	}
	return nil
}

func joinErrs(errs ...error) error {
	var joinErrsR func(string, int, ...error) error
	joinErrsR = func(soFar string, count int, errs ...error) error {
		if len(errs) == 0 {
			if count == 0 {
				return nil
			}
			return fmt.Errorf(soFar)
		}
		current := errs[0]
		next := errs[1:]
		if current == nil {
			return joinErrsR(soFar, count, next...)
		}
		count++
		if count == 1 {
			return joinErrsR(fmt.Sprintf("%s", current), count, next...)
		} else if count == 2 {
			return joinErrsR(fmt.Sprintf("1: %s\n2: %s", soFar, current), count, next...)
		}
		return joinErrsR(fmt.Sprintf("%s\n%d: %s", soFar, count, current), count, next...)
	}
	return joinErrsR("", 0, errs...)
}

type Message struct {
	p     *Peer
	event string
	id    uint64
	msg   []byte
}

func (m *Message) Payload() []byte {
	return m.msg
}

func (m *Message) Reply(msg []byte) error {
	return m.p.reply(m.id, m.event, msg)
}
