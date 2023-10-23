package routing

import (
	"context"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/rpc"
)

type topicFormatter struct{}

func NewTopicFormatter() rpc.TopicFormatter {
	return topicFormatter{}
}

func (f topicFormatter) ParticipantTopic(ctx context.Context, roomName livekit.RoomName, identity livekit.ParticipantIdentity) rpc.ParticipantTopic {
	return rpc.FormatParticipantTopic(roomName, identity)
}

func (f topicFormatter) RoomTopic(ctx context.Context, roomName livekit.RoomName) rpc.RoomTopic {
	return rpc.FormatRoomTopic(roomName)
}
