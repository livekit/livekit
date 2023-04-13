package routing

import (
	"context"
	"encoding/json"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

//go:generate go run github.com/maxbrunsfeld/counterfeiter/v6 -generate

// MessageSink is an abstraction for writing protobuf messages and having them read by a MessageSource,
// potentially on a different node via a transport
//
//counterfeiter:generate . MessageSink
type MessageSink interface {
	WriteMessage(msg proto.Message) error
	IsClosed() bool
	Close()
}

//counterfeiter:generate . MessageSource
type MessageSource interface {
	// ReadChan exposes a one way channel to make it easier to use with select
	ReadChan() <-chan proto.Message
	IsClosed() bool
	Close()
}

type ParticipantInit struct {
	Identity             livekit.ParticipantIdentity
	Name                 livekit.ParticipantName
	Reconnect            bool
	ReconnectReason      livekit.ReconnectReason
	AutoSubscribe        bool
	Client               *livekit.ClientInfo
	Grants               *auth.ClaimGrants
	Region               string
	AdaptiveStream       bool
	ID                   livekit.ParticipantID
	SubscriberAllowPause *bool
}

type NewParticipantCallback func(
	ctx context.Context,
	roomName livekit.RoomName,
	pi ParticipantInit,
	requestSource MessageSource,
	responseSink MessageSink,
) error

type RTCMessageCallback func(
	ctx context.Context,
	roomName livekit.RoomName,
	identity livekit.ParticipantIdentity,
	msg *livekit.RTCNodeMessage,
)

// Router allows multiple nodes to coordinate the participant session
//
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

	GetRegion() string

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

func CreateRouter(config *config.Config, rc redis.UniversalClient, node LocalNode, signalClient SignalClient) Router {
	lr := NewLocalRouter(node, signalClient)

	if rc != nil {
		return NewRedisRouter(config, lr, rc)
	}

	// local routing and store
	logger.Infow("using single-node routing")
	return lr
}

func (pi *ParticipantInit) ToStartSession(roomName livekit.RoomName, connectionID livekit.ConnectionID) (*livekit.StartSession, error) {
	claims, err := json.Marshal(pi.Grants)
	if err != nil {
		return nil, err
	}

	ss := &livekit.StartSession{
		RoomName: string(roomName),
		Identity: string(pi.Identity),
		Name:     string(pi.Name),
		// connection id is to allow the RTC node to identify where to route the message back to
		ConnectionId:    string(connectionID),
		Reconnect:       pi.Reconnect,
		ReconnectReason: pi.ReconnectReason,
		AutoSubscribe:   pi.AutoSubscribe,
		Client:          pi.Client,
		GrantsJson:      string(claims),
		AdaptiveStream:  pi.AdaptiveStream,
		ParticipantId:   string(pi.ID),
	}
	if pi.SubscriberAllowPause != nil {
		subscriberAllowPause := *pi.SubscriberAllowPause
		ss.SubscriberAllowPause = &subscriberAllowPause
	}

	return ss, nil
}

func ParticipantInitFromStartSession(ss *livekit.StartSession, region string) (*ParticipantInit, error) {
	claims := &auth.ClaimGrants{}
	if err := json.Unmarshal([]byte(ss.GrantsJson), claims); err != nil {
		return nil, err
	}

	pi := &ParticipantInit{
		Identity:        livekit.ParticipantIdentity(ss.Identity),
		Name:            livekit.ParticipantName(ss.Name),
		Reconnect:       ss.Reconnect,
		ReconnectReason: ss.ReconnectReason,
		Client:          ss.Client,
		AutoSubscribe:   ss.AutoSubscribe,
		Grants:          claims,
		Region:          region,
		AdaptiveStream:  ss.AdaptiveStream,
		ID:              livekit.ParticipantID(ss.ParticipantId),
	}
	if ss.SubscriberAllowPause != nil {
		subscriberAllowPause := *ss.SubscriberAllowPause
		pi.SubscriberAllowPause = &subscriberAllowPause
	}

	return pi, nil
}
