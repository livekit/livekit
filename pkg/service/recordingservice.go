package service

import (
	"context"
	"errors"
	"time"

	livekit "github.com/livekit/protocol/proto"
	"github.com/livekit/protocol/utils"
	"google.golang.org/protobuf/proto"
)

type RecordingService struct {
	mb utils.MessageBus
}

func NewRecordingService(mb utils.MessageBus) *RecordingService {
	return &RecordingService{mb: mb}
}

func (s *RecordingService) StartRecording(ctx context.Context, req *livekit.StartRecordingRequest) (*livekit.RecordingResponse, error) {
	if err := EnsureRecordPermission(ctx); err != nil {
		return nil, twirpAuthError(err)
	}

	if s.mb == nil {
		return nil, errors.New("recording not configured (redis required)")
	}

	// reserve a recorder
	recordingID, err := s.reserveRecorder(ctx, req)
	if err != nil {
		return nil, err
	}

	// start the recording
	err = s.mb.Publish(ctx, utils.StartRecordingChannel(recordingID), nil)
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

	sub, err := s.mb.Subscribe(ctx, utils.ReservationResponseChannel(id))
	if err != nil {
		return "", err
	}
	defer sub.Close()

	if err = s.mb.Publish(ctx, utils.ReservationChannel, string(b)); err != nil {
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

	if s.mb == nil {
		return nil, errors.New("recording not configured (redis required)")
	}

	if err := s.mb.Publish(ctx, utils.EndRecordingChannel(req.RecordingId), nil); err != nil {
		return nil, err
	}

	return &livekit.RecordingResponse{RecordingId: req.RecordingId}, nil
}
