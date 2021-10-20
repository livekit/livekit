package rtc

import (
	"sync"
	"time"

	"github.com/bep/debounce"
	"github.com/livekit/protocol/logger"
	livekit "github.com/livekit/protocol/proto"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/twcc"
	"github.com/pion/webrtc/v3"

	"github.com/livekit/livekit-server/pkg/utils/stats"
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

	lock                  sync.Mutex
	pendingCandidates     []webrtc.ICECandidateInit
	debouncedNegotiate    func(func())
	onOffer               func(offer webrtc.SessionDescription)
	restartAfterGathering bool
	negotiationState      int
}

type TransportParams struct {
	Target        livekit.SignalTarget
	Config        *WebRTCConfig
	Stats         *stats.RoomStatsReporter
	EnabledCodecs []*livekit.Codec
}

func newPeerConnection(params TransportParams) (*webrtc.PeerConnection, *webrtc.MediaEngine, error) {
	var me *webrtc.MediaEngine
	var err error
	if params.Target == livekit.SignalTarget_PUBLISHER {
		me, err = createPubMediaEngine(params.EnabledCodecs)
	} else {
		me, err = createSubMediaEngine()
	}
	if err != nil {
		return nil, nil, err
	}
	se := params.Config.SettingEngine
	se.DisableMediaEngineCopy(true)
	if params.Stats != nil && se.BufferFactory != nil {
		wrapper := &stats.StatsBufferWrapper{
			CreateBufferFunc: se.BufferFactory,
			Stats:            params.Stats.Incoming,
		}
		se.BufferFactory = wrapper.CreateBuffer
	}

	ir := &interceptor.Registry{}
	if params.Stats != nil && params.Target == livekit.SignalTarget_SUBSCRIBER {
		// only capture subscriber for outbound streams
		f := stats.NewStatsInterceptorFactory(params.Stats)
		ir.Add(f)
	}
	if params.Target == livekit.SignalTarget_SUBSCRIBER {
		f, err := twcc.NewHeaderExtensionInterceptor()
		if err == nil {
			ir.Add(f)
		}
	}
	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(me),
		webrtc.WithSettingEngine(se),
		webrtc.WithInterceptorRegistry(ir),
	)
	pc, err := api.NewPeerConnection(params.Config.Configuration)
	return pc, me, err
}

func NewPCTransport(params TransportParams) (*PCTransport, error) {
	pc, me, err := newPeerConnection(params)
	if err != nil {
		return nil, err
	}

	t := &PCTransport{
		pc:                 pc,
		me:                 me,
		debouncedNegotiate: debounce.New(negotiationFrequency),
		negotiationState:   negotiationStateNone,
	}
	t.pc.OnICEGatheringStateChange(func(state webrtc.ICEGathererState) {
		if state == webrtc.ICEGathererStateComplete {
			go func() {
				t.lock.Lock()
				defer t.lock.Unlock()
				if t.restartAfterGathering {
					logger.Debugw("restarting ICE after ICE gathering")
					if err := t.createAndSendOffer(&webrtc.OfferOptions{ICERestart: true}); err != nil {
						logger.Warnw("could not restart ICE", err)
					}
				}
			}()
		}
	})

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
	t.lock.Lock()
	defer t.lock.Unlock()

	if err := t.pc.SetRemoteDescription(sd); err != nil {
		return err
	}

	// negotiated, reset flag
	lastState := t.negotiationState
	t.negotiationState = negotiationStateNone

	for _, c := range t.pendingCandidates {
		if err := t.pc.AddICECandidate(c); err != nil {
			return err
		}
	}
	t.pendingCandidates = nil

	// only initiate when we are the offerer
	if lastState == negotiationRetry && sd.Type == webrtc.SDPTypeAnswer {
		logger.Debugw("re-negotiate after answering")
		if err := t.createAndSendOffer(nil); err != nil {
			logger.Errorw("could not negotiate", err)
		}
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
			logger.Errorw("could not negotiate", err)
		}
	})
}

func (t *PCTransport) CreateAndSendOffer(options *webrtc.OfferOptions) error {
	t.lock.Lock()
	defer t.lock.Unlock()
	return t.createAndSendOffer(options)
}

// creates and sends offer assuming lock has been acquired
func (t *PCTransport) createAndSendOffer(options *webrtc.OfferOptions) error {
	if t.onOffer == nil {
		return nil
	}
	if t.pc.ConnectionState() == webrtc.PeerConnectionStateClosed {
		return nil
	}

	iceRestart := options != nil && options.ICERestart

	// if restart is requested, and we are not ready, then continue afterwards
	if iceRestart {
		if t.pc.ICEGatheringState() == webrtc.ICEGatheringStateGathering {
			logger.Debugw("restart ICE after gathering")
			t.restartAfterGathering = true
			return nil
		}
		logger.Debugw("restarting ICE")
	}

	// when there's an ongoing negotiation, let it finish and not disrupt its state
	if t.negotiationState == negotiationStateClient {
		currentSD := t.pc.CurrentRemoteDescription()
		if iceRestart && currentSD != nil {
			logger.Debugw("recovering from client negotiation state")
			if err := t.pc.SetRemoteDescription(*currentSD); err != nil {
				stats.PromServiceOperationCounter.WithLabelValues("offer", "error", "remote_description").Add(1)
				return err
			}
		} else {
			logger.Debugw("skipping negotiation, trying again later")
			t.negotiationState = negotiationRetry
			return nil
		}
	} else if t.negotiationState == negotiationRetry {
		// already set to retry, we can safely skip this attempt
		return nil
	}

	offer, err := t.pc.CreateOffer(options)
	if err != nil {
		stats.PromServiceOperationCounter.WithLabelValues("offer", "error", "create").Add(1)
		logger.Errorw("could not create offer", err)
		return err
	}

	err = t.pc.SetLocalDescription(offer)
	if err != nil {
		stats.PromServiceOperationCounter.WithLabelValues("offer", "error", "local_description").Add(1)
		logger.Errorw("could not set local description", err)
		return err
	}

	// indicate waiting for client
	t.negotiationState = negotiationStateClient
	t.restartAfterGathering = false

	go t.onOffer(offer)
	return nil
}
