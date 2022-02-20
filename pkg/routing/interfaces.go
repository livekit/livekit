package routing

import (
	"context"

	"github.com/go-redis/redis/v8"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"google.golang.org/protobuf/proto"
)

//go:generate go run github.com/maxbrunsfeld/counterfeiter/v6 -generate

// MessageSink is an abstraction for writing protobuf messages and having them read by a MessageSource,
// potentially on a different node via a transport
//counterfeiter:generate . MessageSink
type MessageSink interface {
	WriteMessage(msg proto.Message) error
	Close()
	OnClose(f func())
}

//counterfeiter:generate . MessageSource
type MessageSource interface {
	// ReadChan exposes a one way channel to make it easier to use with select
	ReadChan() <-chan proto.Message
}

type ParticipantInit struct {
	Identity      livekit.ParticipantIdentity
	Name          livekit.ParticipantName
	Metadata      string
	Reconnect     bool
	Permission    *livekit.ParticipantPermission
	AutoSubscribe bool
	Hidden        bool
	Recorder      bool
	Client        *livekit.ClientInfo
	Grants        *auth.ClaimGrants
}

type NewParticipantCallback func(ctx context.Context, roomName livekit.RoomName, pi ParticipantInit, requestSource MessageSource, responseSink MessageSink)
type RTCMessageCallback func(ctx context.Context, roomName livekit.RoomName, identity livekit.ParticipantIdentity, msg *livekit.RTCNodeMessage)

// Router allows multiple nodes to coordinate the participant session
//counterfeiter:generate . Router
type Router interface {
	MessageRouter

	RegisterNode() error
	UnregisterNode() error
	RemoveDeadNodes() error

	ListNodes() ([]*livekit.Node, error)

	GetNodeForRoom(ctx context.Context, roomName livekit.RoomName) (*livekit.Node, error)
	SetNodeForRoom(ctx context.Context, roomName livekit.RoomName, nodeId livekit.NodeID) error
	ClearRoomState(ctx context.Context, roomName livekit.RoomName) error

	Start() error
	Drain()
	Stop()

	// OnNewParticipantRTC is called to start a new participant's RTC connection
	OnNewParticipantRTC(callback NewParticipantCallback)

	// OnRTCMessage is called to execute actions on the RTC node
	OnRTCMessage(callback RTCMessageCallback)
}

type MessageRouter interface {
	// StartParticipantSignal participant signal connection is ready to start
	StartParticipantSignal(ctx context.Context, roomName livekit.RoomName, pi ParticipantInit) (connectionID livekit.ConnectionID, reqSink MessageSink, resSource MessageSource, err error)

	// Write a message to a participant or room
	WriteParticipantRTC(ctx context.Context, roomName livekit.RoomName, identity livekit.ParticipantIdentity, msg *livekit.RTCNodeMessage) error
	WriteRoomRTC(ctx context.Context, roomName livekit.RoomName, msg *livekit.RTCNodeMessage) error
}

func CreateRouter(rc *redis.Client, node LocalNode) Router {
	if rc != nil {
		return NewRedisRouter(node, rc)
	}

	// local routing and store
	logger.Infow("using single-node routing")
	return NewLocalRouter(node)
}
