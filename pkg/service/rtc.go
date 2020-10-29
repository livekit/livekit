package service

import (
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/proto/livekit"
)

type RTCService struct {
	livekit.UnimplementedRTCServiceServer
	manager *rtc.RoomManager
}

func (s *RTCService) Signal(stream livekit.RTCService_SignalServer) error {
	return nil
}

func NewRTCService(manager *rtc.RoomManager) *RTCService {
	return &RTCService{
		manager: manager,
	}
}
