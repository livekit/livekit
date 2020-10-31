package service

import (
	"encoding/json"
	"io"

	"github.com/pion/webrtc/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/proto/livekit"
)

type RTCService struct {
	livekit.UnimplementedRTCServiceServer
	manager *rtc.RoomManager
}

func NewRTCService(manager *rtc.RoomManager) *RTCService {
	return &RTCService{
		manager: manager,
	}
}

// each client establishes a persistent signaling connection to the server
// when the connection is terminated, WebRTC session shall end
// similarly, when WebRTC connection is disconnected, we'll terminate signaling
func (s *RTCService) Signal(stream livekit.RTCService_SignalServer) error {
	var peer *rtc.WebRTCPeer
	for {
		req, err := stream.Recv()
		if err != nil {
			if peer != nil {
				peer.Close()

				if err == io.EOF {
					return nil
				}

				errStatus, _ := status.FromError(err)
				if errStatus.Code() == codes.Canceled {
					return nil
				}

				logger.GetLogger().Errorf("signaling error", errStatus.Message(), errStatus.Code())
				return err
			}
		}

		switch msg := req.Message.(type) {
		case *livekit.SignalRequest_Join:
			peer, err = s.handleJoin(msg.Join, stream)
		case *livekit.SignalRequest_Negotiate:
			if peer == nil {
				return status.Errorf(codes.FailedPrecondition, "peer has not joined yet")
			}

		}
	}
	return nil
}

func (s *RTCService) handleJoin(join *livekit.JoinRequest, stream livekit.RTCService_SignalServer) (*rtc.WebRTCPeer, error) {
	// join a room
	room := s.manager.GetRoom(join.RoomId)
	if room == nil {
		return nil, status.Errorf(codes.NotFound, "room %s doesn't exist", join.RoomId)
	}
	peer, err := room.Join(join.PeerId, join.Token, join.Offer.Sdp)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "could not join room: %v", err)
	}

	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  string(join.Offer.Sdp),
	}
	answer, err := peer.Answer(offer)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "could not answer offer: %v", err)
	}

	// TODO: it might be better to return error instead of nil
	peer.OnICECandidate = func(c *webrtc.ICECandidateInit) {
		bytes, err := json.Marshal(c)
		if err != nil {
			logger.GetLogger().Errorf("could not marshal ice candidate: %v", err)
			return
		}

		err = stream.Send(&livekit.SignalResponse{
			Message: &livekit.SignalResponse_Trickle{
				Trickle: &livekit.Trickle{
					CandidateInit: string(bytes),
				},
			},
		})
		if err != nil {
			logger.GetLogger().Errorw("could not send trickle", "err", err)
		}
	}

	// send peer new offer
	peer.OnOffer = func(o webrtc.SessionDescription) {
		err := stream.Send(&livekit.SignalResponse{
			Message: &livekit.SignalResponse_Negotiate{
				Negotiate: ToProtoSessionDescription(&o),
			},
		})
		if err != nil {
			logger.GetLogger().Errorw("could not send offer to peer",
				"err", err)
		}
	}

	// finally send answer
	err = stream.Send(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Join{
			Join: &livekit.JoinResponse{
				Answer: ToProtoSessionDescription(&answer),
			},
		},
	})

	if err != nil {
		return nil, status.Errorf(codes.Internal, "could not join: %v", err)
	}

	return peer, nil
}
