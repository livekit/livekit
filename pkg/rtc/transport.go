package rtc

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/bep/debounce"
	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/pion/webrtc/v3"

	livekit "github.com/livekit/livekit-server/proto"
)

const (
	negotiationFrequency = 150 * time.Millisecond
)

const (
	negotiationStateNone = iota
	// waiting for client answer
	negotiationStateClient
	// need to Negotiate again
	negotiationRetry
)

// PCTransport is a wrapper around PeerConnection, with some helper methods
type PCTransport struct {
	pc *webrtc.PeerConnection
	me *webrtc.MediaEngine

	lock               sync.Mutex
	pendingCandidates  []webrtc.ICECandidateInit
	debouncedNegotiate func(func())
	onOffer            func(offer webrtc.SessionDescription)

	negotiationState atomic.Value
}

func newPeerConnection(target livekit.SignalTarget, conf *WebRTCConfig) (*webrtc.PeerConnection, *webrtc.MediaEngine, error) {
	var me *webrtc.MediaEngine
	var err error
	if target == livekit.SignalTarget_PUBLISHER {
		me, err = createPubMediaEngine()
	} else {
		me, err = createSubMediaEngine()
	}
	if err != nil {
		return nil, nil, err
	}
	se := conf.SettingEngine
	se.DisableMediaEngineCopy(true)

	api := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(conf.Configuration)
	return pc, me, err
}

func NewPCTransport(target livekit.SignalTarget, conf *WebRTCConfig) (*PCTransport, error) {
	pc, me, err := newPeerConnection(target, conf)
	if err != nil {
		return nil, err
	}

	t := &PCTransport{
		pc:                 pc,
		me:                 me,
		debouncedNegotiate: debounce.New(negotiationFrequency),
	}
	t.negotiationState.Store(negotiationStateNone)
	//t.pc.OnNegotiationNeeded(t.Negotiate)

	return t, nil
}

func (t *PCTransport) AddICECandidate(candidate webrtc.ICECandidateInit) error {
	if t.pc.RemoteDescription() == nil {
		t.lock.Lock()
		t.pendingCandidates = append(t.pendingCandidates, candidate)
		t.lock.Unlock()
		return nil
	}

	return t.pc.AddICECandidate(candidate)
}

func (t *PCTransport) PeerConnection() *webrtc.PeerConnection {
	return t.pc
}

func (t *PCTransport) Close() {
	_ = t.pc.Close()
}

func (t *PCTransport) SetRemoteDescription(sd webrtc.SessionDescription) error {
	if err := t.pc.SetRemoteDescription(sd); err != nil {
		return err
	}

	t.lock.Lock()
	for _, c := range t.pendingCandidates {
		if err := t.pc.AddICECandidate(c); err != nil {
			return err
		}
	}
	t.pendingCandidates = nil
	t.lock.Unlock()

	// negotiated, reset flag
	state := t.negotiationState.Load().(int)
	t.negotiationState.Store(negotiationStateNone)
	if state == negotiationRetry {
		// need to Negotiate again
		t.Negotiate()
	}

	return nil
}

// OnOffer is called when the PeerConnection starts negotiation and prepares an offer
func (t *PCTransport) OnOffer(f func(sd webrtc.SessionDescription)) {
	t.onOffer = f
}

func (t *PCTransport) Negotiate() {
	t.debouncedNegotiate(func() {
		if err := t.CreateAndSendOffer(nil); err != nil {
			logger.Errorw("could not negotiate", "error", err)
		}
	})
}

func (t *PCTransport) CreateAndSendOffer(options *webrtc.OfferOptions) error {
	if t.onOffer == nil {
		return nil
	}

	state := t.negotiationState.Load().(int)
	// when there's an ongoing negotiation, let it finish and not disrupt its state
	if state == negotiationStateClient {
		logger.Debugw("skipping negotiation, trying again later")
		t.negotiationState.Store(negotiationRetry)
		return nil
	}

	offer, err := t.pc.CreateOffer(options)
	if err != nil {
		logger.Errorw("could not create offer", "err", err)
		return err
	}

	err = t.pc.SetLocalDescription(offer)
	if err != nil {
		logger.Errorw("could not set local description", "err", err)
		return err
	}

	// indicate waiting for client
	t.negotiationState.Store(negotiationStateClient)

	t.onOffer(offer)
	return nil
}
