package service

import (
	"context"
	"net/http"

	"github.com/livekit/livekit-server/proto/livekit"
)

type RTCService struct {
}

func (s *RTCService) Join(ctx context.Context, req *livekit.JoinRequest) (res *livekit.JoinResponse, err error) {
	return
}

func (s *RTCService) Offer(ctx context.Context, offer *livekit.SessionDescription) (answer *livekit.SessionDescription, err error) {
	return
}

func (s *RTCService) Trickle(ctx context.Context, req *livekit.TrickleRequest) (res *livekit.TrickleResponse, err error) {
	return
}

func (s *RTCService) Signal(w http.ResponseWriter, r *http.Request) {

}

func NewRTCService() *RTCService {
	return &RTCService{}
}
