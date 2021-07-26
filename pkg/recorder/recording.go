package recorder

import (
	"context"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/pkg/errors"
	"google.golang.org/protobuf/proto"

	livekit "github.com/livekit/livekit-server/proto"
)

type RecordingService struct {
	ctx context.Context
	rc  *redis.Client
}

const (
	ReservationChannel = "RESERVE_RECORDER"
	recorderTimeout    = time.Second * 3
)

func NewRecordingService(rc *redis.Client) *RecordingService {
	return &RecordingService{
		ctx: context.Background(),
		rc:  rc,
	}
}

func (s *RecordingService) ReserveRecording(req *livekit.StartRoomRecording) error {
	msg, err := proto.Marshal(req)
	if err != nil {
		return err
	}

	sub := s.rc.Subscribe(s.ctx, ResponseChannel(req.Id))
	defer sub.Close()

	err = s.rc.Publish(s.ctx, ReservationChannel, msg).Err()
	if err != nil {
		return err
	}

	select {
	case <-sub.Channel():
		return nil
	case <-time.After(recorderTimeout):
		return errors.New("no recorders available")
	}
}

func (s *RecordingService) StartRecording(recordingID string) error {
	return s.rc.Publish(s.ctx, StartRecordingChannel(recordingID), nil).Err()
}

func (s *RecordingService) EndRecording(recordingID string) error {
	return s.rc.Publish(s.ctx, EndRecordingChannel(recordingID), nil).Err()
}

func ResponseChannel(id string) string {
	return "RESPONSE" + id
}

func StartRecordingChannel(id string) string {
	return "START_RECORDING_" + id
}

func EndRecordingChannel(id string) string {
	return "END_RECORDING_" + id
}
