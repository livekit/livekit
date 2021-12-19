package service

import (
	"context"
	"errors"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/recording"
	"github.com/livekit/protocol/utils"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/livekit/livekit-server/pkg/telemetry"
)

type RecordingService struct {
	bus       utils.MessageBus
	telemetry telemetry.TelemetryService
	shutdown  chan struct{}
}

func NewRecordingService(mb utils.MessageBus, telemetry telemetry.TelemetryService) *RecordingService {
	return &RecordingService{
		bus:       mb,
		telemetry: telemetry,
		shutdown:  make(chan struct{}, 1),
	}
}

func (s *RecordingService) Start() {
	if s.bus != nil {
		go s.resultsWorker()
	}
}

func (s *RecordingService) Stop() {
	s.shutdown <- struct{}{}
}

func (s *RecordingService) StartRecording(ctx context.Context, req *livekit.StartRecordingRequest) (*livekit.StartRecordingResponse, error) {
	if err := EnsureRecordPermission(ctx); err != nil {
		return nil, twirpAuthError(err)
	}
	if s.bus == nil {
		return nil, errors.New("recording not configured (redis required)")
	}

	// reserve a recorder
	recordingID, err := recording.ReserveRecorder(s.bus)
	if err != nil {
		return nil, err
	}

	// start the recording
	err = recording.RPC(ctx, s.bus, recordingID, &livekit.RecordingRequest{
		Request: &livekit.RecordingRequest_Start{
			Start: req,
		},
	})
	if err != nil {
		return nil, err
	}

	ri := &livekit.RecordingInfo{
		Id:     recordingID,
		Active: true,
	}
	if template := req.Input.(*livekit.StartRecordingRequest_Template); template != nil {
		ri.RoomName = template.Template.RoomName
	}
	logger.Debugw("recording started", "recordingID", recordingID)
	s.telemetry.RecordingStarted(ctx, ri)

	return &livekit.StartRecordingResponse{RecordingId: recordingID}, nil
}

func (s *RecordingService) AddOutput(ctx context.Context, req *livekit.AddOutputRequest) (*emptypb.Empty, error) {
	if err := EnsureRecordPermission(ctx); err != nil {
		return nil, twirpAuthError(err)
	}
	if s.bus == nil {
		return nil, errors.New("recording not configured (redis required)")
	}

	err := recording.RPC(ctx, s.bus, req.RecordingId, &livekit.RecordingRequest{
		Request: &livekit.RecordingRequest_AddOutput{
			AddOutput: req,
		},
	})
	if err != nil {
		return nil, err
	}

	return &emptypb.Empty{}, nil
}

func (s *RecordingService) RemoveOutput(ctx context.Context, req *livekit.RemoveOutputRequest) (*emptypb.Empty, error) {
	if err := EnsureRecordPermission(ctx); err != nil {
		return nil, twirpAuthError(err)
	}
	if s.bus == nil {
		return nil, errors.New("recording not configured (redis required)")
	}

	err := recording.RPC(ctx, s.bus, req.RecordingId, &livekit.RecordingRequest{
		Request: &livekit.RecordingRequest_RemoveOutput{
			RemoveOutput: req,
		},
	})
	if err != nil {
		return nil, err
	}

	return &emptypb.Empty{}, nil
}

func (s *RecordingService) EndRecording(ctx context.Context, req *livekit.EndRecordingRequest) (*emptypb.Empty, error) {
	if err := EnsureRecordPermission(ctx); err != nil {
		return nil, twirpAuthError(err)
	}
	if s.bus == nil {
		return nil, errors.New("recording not configured (redis required)")
	}

	err := recording.RPC(ctx, s.bus, req.RecordingId, &livekit.RecordingRequest{
		Request: &livekit.RecordingRequest_End{
			End: req,
		},
	})
	if err != nil {
		return nil, err
	}

	return &emptypb.Empty{}, nil
}

func (s *RecordingService) resultsWorker() {
	sub, err := s.bus.SubscribeQueue(context.Background(), recording.ResultChannel)
	if err != nil {
		logger.Errorw("failed to subscribe to results channel", err)
		return
	}

	resChan := sub.Channel()
	for {
		select {
		case msg := <-resChan:
			b := sub.Payload(msg)

			res := &livekit.RecordingInfo{}
			if err = proto.Unmarshal(b, res); err != nil {
				logger.Errorw("failed to read results", err)
				continue
			}

			// log results
			values := []interface{}{"recordingID", res.Id}
			if res.Error != "" {
				values = append(values, "error", res.Error)
			}
			logger.Debugw("recording ended", values...)

			s.telemetry.RecordingEnded(context.Background(), res)
		case <-s.shutdown:
			_ = sub.Close()
			return
		}
	}
}
