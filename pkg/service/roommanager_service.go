package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pion/webrtc/v4"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu/rtpstats"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"
)

const (
	whipSessionNotifyInterval = 10 * time.Second
)

type whipService struct {
	*RoomManager

	ingressRpcCli rpc.IngressHandlerClient

	rpc.UnimplementedWHIPServer
}

func newWhipService(rm *RoomManager) (*whipService, error) {
	cli, err := rpc.NewIngressHandlerClient(rm.bus, rpc.WithDefaultClientOptions(logger.GetLogger()))
	if err != nil {
		return nil, err
	}
	return &whipService{
		RoomManager:   rm,
		ingressRpcCli: cli,
	}, nil
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

	if err := lp.HandleOffer(&livekit.SessionDescription{
		Type: webrtc.SDPTypeOffer.String(),
		Sdp:  req.OfferSdp,
		Id:   0,
	}); err != nil {
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

	if req.FromIngress {
		aliveCtx, cancel := context.WithCancel(context.Background())

		lp.AddOnClose(types.ParticipantCloseKeyWHIP, func(lp types.LocalParticipant) {
			cancel()

			go func() {
				lp.GetLogger().Debugw("whip service: notify participant closed")

				video, audio := getMediaStateForParticipant(lp)

				_, err := s.ingressRpcCli.WHIPRTCConnectionNotify(context.Background(), string(lp.ID()), &rpc.WHIPRTCConnectionNotifyRequest{
					ParticipantId: string(lp.ID()),
					Closed:        true,
					Audio:         audio,
					Video:         video,
				}, psrpc.WithRequestTimeout(rpc.DefaultPSRPCConfig.Timeout))
				if err != nil {
					lp.GetLogger().Warnw("whip service: could not notify ingress of participant closed", err)
				}
			}()
		})
		go func() {
			if err := s.notifySession(aliveCtx, lp); err != nil {
				cancel()
			}
		}()
	}

	var iceServers []*livekit.ICEServer
	apiKey, _, err := s.RoomManager.getFirstKeyPair()
	if err == nil {
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

func (s whipService) notifySession(ctx context.Context, participant types.Participant) error {
	ticker := time.NewTicker(whipSessionNotifyInterval)
	defer ticker.Stop()

	err := s.sendConnectionNotify(ctx, participant)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
	}

	for {
		select {
		case <-ticker.C:
			err := s.sendConnectionNotify(ctx, participant)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return nil
				}
			}

		case <-ctx.Done():
			return nil
		}
	}
}

func (s whipService) sendConnectionNotify(ctx context.Context, participant types.Participant) error {
	video, audio := getMediaStateForParticipant(participant)

	_, err := s.ingressRpcCli.WHIPRTCConnectionNotify(ctx, string(participant.ID()), &rpc.WHIPRTCConnectionNotifyRequest{
		ParticipantId: string(participant.ID()),
		Video:         video,
		Audio:         audio,
	}, psrpc.WithRequestTimeout(rpc.DefaultPSRPCConfig.Timeout))

	return err
}

func getMediaStateForParticipant(participant types.Participant) (*livekit.InputVideoState, *livekit.InputAudioState) {
	var video *livekit.InputVideoState
	var audio *livekit.InputAudioState

	for _, t := range participant.GetPublishedTracks() {
		if t == nil {
			continue
		}

		ti := t.ToProto()
		if ti == nil {
			continue
		}

		switch t.Kind() {
		case livekit.TrackType_VIDEO:
			if video != nil {
				continue
			}

			video = &livekit.InputVideoState{
				MimeType:       ti.MimeType,
				Width:          ti.Width,
				Height:         ti.Height,
				AverageBitrate: trackAverageBitrate(t),
			}

		case livekit.TrackType_AUDIO:
			if audio != nil {
				continue
			}

			channels := uint32(1)
			if ti.Stereo {
				channels = 2
			}

			audio = &livekit.InputAudioState{
				MimeType:       ti.MimeType,
				Channels:       channels,
				AverageBitrate: trackAverageBitrate(t),
			}
		}
	}

	return video, audio
}

func trackAverageBitrate(t types.MediaTrack) uint32 {
	var allStats []*livekit.RTPStats
	for _, r := range t.Receivers() {
		if r == nil {
			continue
		}

		if s := r.GetTrackStats(); s != nil {
			allStats = append(allStats, s)
		}
	}

	agg := rtpstats.AggregateRTPStats(allStats)
	if agg == nil {
		return 0
	}

	return uint32(agg.Bitrate)
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
		lp.AddOnClose(types.ParticipantCloseKeyWHIP, nil)
		room.RemoveParticipant(
			lp.Identity(),
			lp.ID(),
			types.ParticipantCloseReasonClientRequestLeave,
		)
	}

	return &emptypb.Empty{}, nil
}

// --------------------------------
