package service

import (
	"context"
	"errors"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/livekit/protocol/utils"
	"google.golang.org/protobuf/proto"

	livekit "github.com/livekit/livekit-server/proto"
)

type RecordingService struct {
	rc *redis.Client
}

func NewRecordingService(rc *redis.Client) *RecordingService {
	return &RecordingService{rc: rc}
}

func (s *RecordingService) StartRecording(ctx context.Context, req *livekit.StartRecordingRequest) (*livekit.RecordingResponse, error) {
	if err := EnsureRecordPermission(ctx); err != nil {
		return nil, twirpAuthError(err)
	}

	if s.rc == nil {
		return nil, errors.New("recording not configured (redis required)")
	}

	// reserve a recorder
	recordingID, err := s.reserveRecorder(ctx, req)
	if err != nil {
		return nil, err
	}

	// start the recording
	err = s.rc.Publish(ctx, utils.StartRecordingChannel(recordingID), nil).Err()
	if err != nil {
		return nil, err
	}

	return &livekit.RecordingResponse{RecordingId: recordingID}, nil
}

func (s *RecordingService) reserveRecorder(ctx context.Context, req *livekit.StartRecordingRequest) (string, error) {
	id := utils.NewGuid(utils.RecordingPrefix)
	reservation := &livekit.RecordingReservation{
		Id:          id,
		SubmittedAt: time.Now().UnixNano(),
		Request:     req,
	}
	b, err := proto.Marshal(reservation)
	if err != nil {
		return "", err
	}

	sub := s.rc.Subscribe(ctx, utils.ReservationResponseChannel(id))
	defer sub.Close()

	if err = s.rc.Publish(ctx, utils.ReservationChannel, string(b)).Err(); err != nil {
		return "", err
	}

	select {
	case <-sub.Channel():
		return id, nil
	case <-time.After(utils.RecorderTimeout):
		return "", errors.New("recording request failed")
	}
}

func (s *RecordingService) EndRecording(ctx context.Context, req *livekit.EndRecordingRequest) (*livekit.RecordingResponse, error) {
	if err := EnsureRecordPermission(ctx); err != nil {
		return nil, twirpAuthError(err)
	}

	if s.rc == nil {
		return nil, errors.New("recording not configured (redis required)")
	}

	if err := s.rc.Publish(ctx, utils.EndRecordingChannel(req.RecordingId), nil).Err(); err != nil {
		return nil, err
	}

	return &livekit.RecordingResponse{RecordingId: req.RecordingId}, nil
}
