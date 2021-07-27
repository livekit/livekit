package recording

import (
	"context"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/pkg/errors"
)

type RoomRecorder struct {
	rc *redis.Client
}

const (
	ReservationChannel = "RESERVE_RECORDER"
	recorderTimeout    = time.Second * 3
	// used by livekit-recorder
	ReservationTimeout = time.Second * 2
)

func NewRoomRecorder(rc *redis.Client) *RoomRecorder {
	if rc == nil {
		return nil
	}
	return &RoomRecorder{rc: rc}
}

func (s *RoomRecorder) ReserveRecorder(ctx context.Context, msg, id string) error {
	sub := s.rc.Subscribe(ctx, ResponseChannel(id))
	defer sub.Close()

	err := s.rc.Publish(ctx, ReservationChannel, msg).Err()
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

func (s *RoomRecorder) StartRecording(ctx context.Context, recordingID string) error {
	return s.rc.Publish(ctx, StartRecordingChannel(recordingID), nil).Err()
}

func (s *RoomRecorder) EndRecording(ctx context.Context, recordingID string) error {
	return s.rc.Publish(ctx, EndRecordingChannel(recordingID), nil).Err()
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
