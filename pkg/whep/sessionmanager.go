package whep

import (
	"context"
	"sync"

	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v3"

	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/tracer"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/psrpc"
)

type SessionManager struct {
	bus         psrpc.MessageBus
	sessionLock sync.Mutex
	sessions    map[string]*Session
	rtcAPI      *webrtc.API
	logger      logger.Logger
	psrpcServer rpc.WHEPInternalServer
}

func NewSessionManager(bus psrpc.MessageBus, rtccfg rtcconfig.WebRTCConfig) (*SessionManager, error) {
	var me webrtc.MediaEngine
	var ir interceptor.Registry
	me.RegisterDefaultCodecs()
	if err := webrtc.ConfigureNack(&me, &ir); err != nil {
		return nil, err
	}

	if err := webrtc.ConfigureRTCPReports(&ir); err != nil {
		return nil, err
	}

	s := &SessionManager{
		bus:      bus,
		logger:   logger.GetLogger(),
		sessions: make(map[string]*Session),
		rtcAPI: webrtc.NewAPI(
			webrtc.WithSettingEngine(rtccfg.SettingEngine),
			webrtc.WithMediaEngine(&me),
			webrtc.WithInterceptorRegistry(&ir),
		),
	}
	psrpcServer, err := rpc.NewWHEPInternalServer(s, bus)
	if err != nil {
		return nil, err
	}
	s.psrpcServer = psrpcServer
	return s, nil
}

func (sm *SessionManager) StartWHEP(ctx context.Context, req *rpc.StartWHEPRequest) (*rpc.StartWHEPResponse, error) {
	_, span := tracer.Start(ctx, "WHEPSessionManager.StartWHEP")
	defer span.End()

	sm.logger.Debugw("handle StartWhEP", "participant", req.Participant, "offer", req.Offer)
	resourceID := utils.NewGuid(utils.WHIPResourcePrefix)
	session := NewSession(SessionParams{
		Participant: req.Participant,
		Token:       req.Token,
		Offer:       req.Offer,
		WsUrl:       req.WsUrl,
		RtcAPI:      sm.rtcAPI,
		Logger:      sm.logger.WithValues("resourceID", resourceID),
	})

	answer, err := session.CreateAnswer()
	if err != nil {
		session.Close()
		session.params.Logger.Warnw("failed to create answer", err)
		return nil, err
	}

	rpcServer, err := rpc.NewWHEPHandlerServer(session, sm.bus)
	if err != nil {
		session.Close()
		return nil, err
	}
	if err = rpcServer.RegisterAllResuourceTopics(resourceID); err != nil {
		rpcServer.Shutdown()
		session.Close()
		return nil, err
	}
	sm.sessionLock.Lock()
	sm.sessions[resourceID] = session
	sm.sessionLock.Unlock()

	session.OnClose(func() {
		rpcServer.DeregisterAllResuourceTopics(resourceID)
		sm.sessionLock.Lock()
		delete(sm.sessions, resourceID)
		sm.sessionLock.Unlock()
	})

	sm.logger.Debugw("WHEP session started", "resourceID", resourceID, "answer", answer)

	return &rpc.StartWHEPResponse{
		ResourceId: resourceID,
		Answer:     answer,
	}, nil
}

func (sm *SessionManager) StartWHEPAffinity(context.Context, *rpc.StartWHEPRequest) float32 {
	// TODO: if NoCapacity return -1; if hasRoom return 1; else return 0
	return 1

}

func (sm *SessionManager) Stop() {
	sm.psrpcServer.Shutdown()
	sm.sessionLock.Lock()
	sessions := sm.sessions
	sm.sessions = make(map[string]*Session)
	sm.sessionLock.Unlock()
	for _, s := range sessions {
		s.Close()
	}
}
