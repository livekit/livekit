package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/livekit/protocol/logger"
	livekit "github.com/livekit/protocol/proto"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/protocol/webhook"
	"google.golang.org/protobuf/proto"
)

type RecordingService struct {
	mb       utils.MessageBus
	notifier *webhook.Notifier
}

func NewRecordingService(mb utils.MessageBus, notifier *webhook.Notifier) *RecordingService {
	s := &RecordingService{
		mb:       mb,
		notifier: notifier,
	}
	go s.resultsWorker()
	return s
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

func (s *RecordingService) resultsWorker() {
	sub, err := s.mb.Subscribe(context.Background(), utils.RecordingResultChannel)
	if err != nil {
		logger.Errorw("failed to subscribe to results channel", err)
		return
	}

	resChan := sub.Channel()
	for {
		msg := <-resChan
		b := sub.Payload(msg)

		res := &livekit.RecordingResult{}
		if err = proto.Unmarshal(b, res); err != nil {
			logger.Errorw("failed to read results", err)
			continue
		}
		s.notify(res)
	}
}

func (s *RecordingService) notify(res *livekit.RecordingResult) {
	acquired, err := s.mb.Lock(context.Background(), res.Id, time.Second*5)
	if err != nil {
		logger.Errorw("failed to lock", err)
		return
	}
	if !acquired {
		return
	}

	// log results
	if res.Error != "" {
		logger.Errorw("recording failed", errors.New(res.Error), "id", res.Id)
	} else {
		logger.Infow("recording complete",
			"id", res.Id,
			"duration", fmt.Sprint(time.Duration(res.Duration*1e6)),
			"location", res.Location,
		)
	}

	// webhook
	if s.notifier != nil {
		event := webhook.EventRecordingFinished
		if err := s.notifier.Notify(&livekit.WebhookEvent{
			Event:           event,
			RecordingResult: res,
		}); err != nil {
			logger.Warnw("could not notify webhook", err, "event", event)
		}
	}
}
