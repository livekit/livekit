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

type whipService struct {
	*RoomManager
}

func (s whipService) Create(ctx context.Context, req *rpc.WHIPCreateRequest) (*rpc.WHIPCreateResponse, error) {
	pi, err := routing.ParticipantInitFromStartSession(req.StartSession, s.RoomManager.currentNode.Region())
	if err != nil {
		logger.Errorw("whip service: could not create participant init", err)
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
		logger.Errorw("whip service: could not start session", err)
		return nil, err
	}

	room := s.RoomManager.GetRoom(ctx, livekit.RoomName(req.StartSession.RoomName))
	if room == nil {
		logger.Errorw("whip service: could not find room", nil, "room", req.StartSession.RoomName)
		return nil, ErrRoomNotFound
	}

	lp := room.GetParticipant(pi.Identity)
	if lp == nil {
		room.Logger().Errorw("whip service: could not find local participant", nil, "participant", pi.Identity)
		return nil, ErrParticipantNotFound
	}

	if err := lp.HandleOffer(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: req.OfferSdp}, 0); err != nil {
		lp.GetLogger().Errorw("whip service: could not handle offer", err)
		return nil, err
	}

	// wait for subscriptions to resolve
	// NOTE: this is outside the WHIP spec, but added as a convenience for clients doing
	// one-shot signalling (i. e. send an offer and get an answer once) to publish and subscribe to
	// well-known tracks (i. e. remote participant identity and track names are well known)
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
		lp.GetLogger().Errorw("whip service: could not subscribe to tracks", err)
		return nil, err
	}

	answer, _, err := lp.GetAnswer()
	if err != nil {
		lp.GetLogger().Errorw("whip service: could not get answer", err)
		return nil, err
	}

	iceSessionID, err := lp.GetPublisherICESessionUfrag()
	if err != nil {
		lp.GetLogger().Errorw("whip service: could not get ICE session ID", err)
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
	return &rpc.WHIPCreateResponse{
		AnswerSdp:     answer.SDP,
		ParticipantId: string(lp.ID()),
		IceServers:    iceServers,
		IceSessionId:  iceSessionID,
	}, nil
}

// -------------------------------------------

type whipParticipantService struct {
	*RoomManager
}

func (r whipParticipantService) ICETrickle(ctx context.Context, req *rpc.WHIPParticipantICETrickleRequest) (*emptypb.Empty, error) {
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
		lp.GetLogger().Warnw("whipParticipant service ice-trickle: could not get ICE session ufrag", err)
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

func (r whipParticipantService) ICERestart(ctx context.Context, req *rpc.WHIPParticipantICERestartRequest) (*rpc.WHIPParticipantICERestartResponse, error) {
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
		lp.GetLogger().Warnw("whipParticipant service ice-restart: could not get ICE session ufrag", err)
		return nil, psrpc.NewError(psrpc.Internal, err)
	}

	return &rpc.WHIPParticipantICERestartResponse{
		IceSessionId: iceSessionID,
		SdpFragment:  sdpFragment,
	}, nil
}

func (r whipParticipantService) DeleteSession(ctx context.Context, req *rpc.WHIPParticipantDeleteSessionRequest) (*emptypb.Empty, error) {
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

// --------------------------------

type signalv2ParticipantService struct {
	*RoomManager
}

func (s signalv2ParticipantService) RelaySignalv2Participant(ctx context.Context, req *rpc.RelaySignalv2ParticipantRequest) (*rpc.RelaySignalv2ParticipantResponse, error) {
	room := s.RoomManager.GetRoom(ctx, livekit.RoomName(req.Room))
	if room == nil {
		return nil, ErrRoomNotFound
	}

	lp := room.GetParticipantByID(livekit.ParticipantID(req.ParticipantId))
	if lp == nil {
		return nil, ErrParticipantNotFound
	}

	err := lp.HandleSignalMessage(req.WireMessage)
	if err != nil {
		return nil, err
	}

	var wireMessage *livekit.Signalv2WireMessage
	if pending := lp.SignalPendingMessages(); pending != nil {
		var ok bool
		if wireMessage, ok = pending.(*livekit.Signalv2WireMessage); !ok {
			return nil, ErrInvalidMessageType
		}
	}

	return &rpc.RelaySignalv2ParticipantResponse{
		WireMessage: wireMessage,
	}, nil
}

func (s signalv2ParticipantService) RelaySignalv2ParticipantDeleteSession(
	ctx context.Context,
	req *rpc.RelaySignalv2ParticipantDeleteSessionRequest,
) (*emptypb.Empty, error) {
	room := s.RoomManager.GetRoom(ctx, livekit.RoomName(req.Room))
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
