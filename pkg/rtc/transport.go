package rtc

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/bep/debounce"
	"github.com/pion/webrtc/v3"

	"github.com/livekit/livekit-server/proto/livekit"
)

const (
	negotiationFrequency = 150 * time.Millisecond
)

const (
	negotiationStateNone = iota
	// waiting for client answer
	negotiationStateClient
	// need to negotiate again
	negotiationStateServer
)

// PCTransport is a wrapper around PeerConnection, with some helper methods
type PCTransport struct {
	//kind livekit.SignalTarget
	pc *webrtc.PeerConnection
	me *webrtc.MediaEngine

	lock              sync.Mutex
	pendingCandidates []webrtc.ICECandidateInit
	onNegotiation     func()

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
	se.BufferFactory = bufferFactory.GetOrNew

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
		pc: pc,
		me: me,
	}
	t.negotiationState.Store(negotiationStateNone)

	debounced := debounce.New(negotiationFrequency)
	t.pc.OnNegotiationNeeded(func() {
		debounced(func() {
			state := t.negotiationState.Load().(int)
			// when there's an ongoing negotiation, let it finish and not disrupt its state
			if state != negotiationStateNone {
				t.negotiationState.Store(negotiationStateServer)
				return
			}

			if t.onNegotiation != nil {
				t.onNegotiation()

				// indicate waiting for client
				t.negotiationState.Store(negotiationStateClient)
			}
		})
	})

	return t, nil
}

func (t *PCTransport) AddICECandidate(candidate webrtc.ICECandidateInit) error {
	if t.pc.RemoteDescription() == nil {
		t.pendingCandidates = append(t.pendingCandidates, candidate)
		return nil
	}

	return t.pc.AddICECandidate(candidate)
}

func (t *PCTransport) PeerConnection() *webrtc.PeerConnection {
	return t.pc
}

func (t *PCTransport) Close() {
	t.pc.Close()
}

func (t *PCTransport) SetRemoteDescription(sd webrtc.SessionDescription) error {
	if err := t.pc.SetRemoteDescription(sd); err != nil {
		return err
	}

	for _, c := range t.pendingCandidates {
		if err := t.pc.AddICECandidate(c); err != nil {
			return err
		}
	}
	t.pendingCandidates = nil

	// negotiated, reset flag
	state := t.negotiationState.Load().(int)
	t.negotiationState.Store(negotiationStateNone)
	if state == negotiationStateServer && t.onNegotiation != nil {
		// need to negotiate again
		t.onNegotiation()
	}

	return nil
}

func (t *PCTransport) OnNegotiationNeeded(f func()) {
	t.onNegotiation = f
}
