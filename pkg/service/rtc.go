package service

import (
	"context"
	"net/http"

	"github.com/livekit/livekit-server/proto"
)

type RTCService struct {
}

func (s *RTCService) Offer(ctx context.Context, offer *proto.SessionDescription) (answer *proto.SessionDescription, err error) {
	return
}

func (s *RTCService) Trickle(ctx context.Context, req *proto.TrickleRequest) (res *proto.TrickleResponse, err error) {
	return
}

func (s *RTCService) Signal(w http.ResponseWriter, r *http.Request) {

}

func NewRTCService() *RTCService {
	return &RTCService{}
}
