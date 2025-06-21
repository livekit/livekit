package service

import (
	"context"
	"fmt"
	"time"

	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"
	"github.com/pion/webrtc/v4"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/types/known/emptypb"
)

type rtcRestService struct {
	*RoomManager
}

func (s rtcRestService) Create(ctx context.Context, req *rpc.RTCRestCreateRequest) (*rpc.RTCRestCreateResponse, error) {
	pi, err := routing.ParticipantInitFromStartSession(req.StartSession, s.RoomManager.currentNode.Region())
	if err != nil {
		logger.Errorw("rtcRest service: could not create participant init", err)
		return nil, err
	}

	prometheus.IncrementParticipantRtcInit(1)

	if err = s.RoomManager.StartSession(
		ctx,
		*pi,
		routing.NewNullMessageSource(livekit.ConnectionID(req.StartSession.ConnectionId)), // no requestSource
		routing.NewNullMessageSink(livekit.ConnectionID(req.StartSession.ConnectionId)),   // no responseSink
		true, // useOneShotSignallingMode
	); err != nil {
		logger.Errorw("rtcRest service: could not start session", err)
		return nil, err
	}

	room := s.RoomManager.GetRoom(ctx, livekit.RoomName(req.StartSession.RoomName))
	if room == nil {
		logger.Errorw("rtcRest service: could not find room", nil, "room", req.StartSession.RoomName)
		return nil, ErrRoomNotFound
	}

	lp := room.GetParticipant(pi.Identity)
	if lp == nil {
		room.Logger().Errorw("rtcRest service: could not find local participant", nil, "participant", pi.Identity)
		return nil, ErrParticipantNotFound
	}

	if err := lp.HandleOffer(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: req.OfferSdp}, 0); err != nil {
		lp.GetLogger().Errorw("rtcRest service: could not handle offer", err)
		return nil, err
	}

	// wait for subscriptions to resolve
	eg, _ := errgroup.WithContext(ctx)
	for publisherIdentity, trackList := range req.SubscribedParticipantTracks {
		for _, trackName := range trackList.TrackNames {
			eg.Go(func() error {
				for {
					if lp.IsTrackNameSubscribed(livekit.ParticipantIdentity(publisherIdentity), trackName) {
						return nil
					}
					time.Sleep(50 * time.Millisecond)
				}
			})
		}
	}
	err = eg.Wait()
	if err != nil {
		lp.GetLogger().Errorw("rtcRest service: could not subscribe to tracks", err)
		return nil, err
	}

	answer, err := lp.GetAnswer()
	if err != nil {
		lp.GetLogger().Errorw("rtcRest service: could not get answer", err)
		return nil, err
	}

	iceSessionID, err := lp.GetPublisherICESessionUfrag()
	if err != nil {
		lp.GetLogger().Errorw("rtcRest service: could not get ICE session ID", err)
		return nil, err
	}

	var iceServers []*livekit.ICEServer
	apiKey, _, err := s.RoomManager.getFirstKeyPair()
	if err != nil {
		iceServers = s.RoomManager.iceServersForParticipant(
			apiKey,
			lp,
			false,
		)
	}
	return &rpc.RTCRestCreateResponse{
		AnswerSdp:     answer.SDP,
		ParticipantId: string(lp.ID()),
		IceServers:    iceServers,
		IceSessionId:  iceSessionID,
	}, nil
}

type rtcRestParticipantService struct {
	*RoomManager
}

func (r rtcRestParticipantService) ICETrickle(ctx context.Context, req *rpc.RTCRestParticipantICETrickleRequest) (*emptypb.Empty, error) {
	room := r.RoomManager.GetRoom(ctx, livekit.RoomName(req.Room))
	if room == nil {
		return nil, ErrRoomNotFound
	}

	lp := room.GetParticipantByID(livekit.ParticipantID(req.ParticipantId))
	if lp == nil {
		return nil, ErrParticipantNotFound
	}

	iceSessionID, err := lp.GetPublisherICESessionUfrag()
	if err != nil {
		lp.GetLogger().Warnw("rtcRestParticipant service ice-trickle: could not get ICE session ufrag", err)
		return nil, psrpc.NewError(psrpc.Internal, err)
	}

	if req.IceSessionId != iceSessionID {
		return nil, psrpc.NewError(
			psrpc.FailedPrecondition,
			fmt.Errorf("ice session mismatch, expected: %s, got: %s", iceSessionID, req.IceSessionId),
		)
	}

	if err := lp.HandleICETrickleSDPFragment(req.SdpFragment); err != nil {
		return nil, psrpc.NewError(psrpc.InvalidArgument, err)
	}

	return &emptypb.Empty{}, nil
}

func (r rtcRestParticipantService) ICERestart(ctx context.Context, req *rpc.RTCRestParticipantICERestartRequest) (*rpc.RTCRestParticipantICERestartResponse, error) {
	room := r.RoomManager.GetRoom(ctx, livekit.RoomName(req.Room))
	if room == nil {
		return nil, ErrRoomNotFound
	}

	lp := room.GetParticipantByID(livekit.ParticipantID(req.ParticipantId))
	if lp == nil {
		return nil, ErrParticipantNotFound
	}

	sdpFragment, err := lp.HandleICERestartSDPFragment(req.SdpFragment)
	if err != nil {
		return nil, psrpc.NewError(psrpc.InvalidArgument, err)
	}

	iceSessionID, err := lp.GetPublisherICESessionUfrag()
	if err != nil {
		lp.GetLogger().Warnw("rtcRestParticipant service ice-restart: could not get ICE session ufrag", err)
		return nil, psrpc.NewError(psrpc.Internal, err)
	}

	return &rpc.RTCRestParticipantICERestartResponse{
		IceSessionId: iceSessionID,
		SdpFragment:  sdpFragment,
	}, nil
}

func (r rtcRestParticipantService) DeleteSession(ctx context.Context, req *rpc.RTCRestParticipantDeleteSessionRequest) (*emptypb.Empty, error) {
	room := r.RoomManager.GetRoom(ctx, livekit.RoomName(req.Room))
	if room == nil {
		return nil, ErrRoomNotFound
	}

	lp := room.GetParticipantByID(livekit.ParticipantID(req.ParticipantId))
	if lp != nil {
		room.RemoveParticipant(
			lp.Identity(),
			lp.ID(),
			types.ParticipantCloseReasonClientRequestLeave,
		)
	}

	return &emptypb.Empty{}, nil
}
