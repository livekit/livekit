package recording

import (
	"context"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/livekit/protocol/utils"
	"github.com/pkg/errors"
	"google.golang.org/protobuf/proto"

	livekit "github.com/livekit/livekit-server/proto"
)

type RoomRecorder struct {
	rc *redis.Client
}

func NewRoomRecorder(rc *redis.Client) *RoomRecorder {
	if rc == nil {
		return nil
	}
	return &RoomRecorder{rc: rc}
}

func (s *RoomRecorder) ReserveRecorder(ctx context.Context, req *livekit.RecordRoomRequest) (string, error) {
	id := utils.NewGuid(utils.RecordingPrefix)
	reservation := &livekit.RecordingReservation{
		Id:          id,
		SubmittedAt: time.Now().UnixNano(),
		Input:       req.Input,
		Output:      req.Output,
	}
	b, err := proto.Marshal(reservation)
	if err != nil {
		return "", err
	}

	sub := s.rc.Subscribe(ctx, utils.ReservationResponseChannel(id))
	defer sub.Close()

	err = s.rc.Publish(ctx, utils.ReservationChannel, string(b)).Err()
	if err != nil {
		return "", err
	}

	select {
	case <-sub.Channel():
		return id, nil
	case <-time.After(utils.RecorderTimeout):
		return "", errors.New("no recorders available")
	}
}

func (s *RoomRecorder) StartRecording(ctx context.Context, recordingID string) error {
	return s.rc.Publish(ctx, utils.StartRecordingChannel(recordingID), nil).Err()
}

func (s *RoomRecorder) EndRecording(ctx context.Context, recordingID string) error {
	return s.rc.Publish(ctx, utils.EndRecordingChannel(recordingID), nil).Err()
}
