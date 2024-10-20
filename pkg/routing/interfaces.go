// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package routing

import (
	"context"
	"encoding/json"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
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
	ConnectionID() livekit.ConnectionID
}

//counterfeiter:generate . MessageSource
type MessageSource interface {
	// ReadChan exposes a one way channel to make it easier to use with select
	ReadChan() <-chan proto.Message
	IsClosed() bool
	Close()
	ConnectionID() livekit.ConnectionID
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
	DisableICELite       bool
	CreateRoom           *livekit.CreateRoomRequest
}

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
}

type StartParticipantSignalResults struct {
	ConnectionID        livekit.ConnectionID
	RequestSink         MessageSink
	ResponseSource      MessageSource
	NodeID              livekit.NodeID
	NodeSelectionReason string
}

type MessageRouter interface {
	// CreateRoom starts an rtc room
	CreateRoom(ctx context.Context, req *livekit.CreateRoomRequest) (res *livekit.Room, err error)
	// StartParticipantSignal participant signal connection is ready to start
	StartParticipantSignal(ctx context.Context, roomName livekit.RoomName, pi ParticipantInit) (res StartParticipantSignalResults, err error)
}

func CreateRouter(
	rc redis.UniversalClient,
	node LocalNode,
	signalClient SignalClient,
	roomManagerClient RoomManagerClient,
	kps rpc.KeepalivePubSub,
) Router {
	lr := NewLocalRouter(node, signalClient, roomManagerClient)

	if rc != nil {
		return NewRedisRouter(lr, rc, kps)
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
		DisableIceLite:  pi.DisableICELite,
		CreateRoom:      pi.CreateRoom,
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
		DisableICELite:  ss.DisableIceLite,
		CreateRoom:      ss.CreateRoom,
	}
	if ss.SubscriberAllowPause != nil {
		subscriberAllowPause := *ss.SubscriberAllowPause
		pi.SubscriberAllowPause = &subscriberAllowPause
	}

	return pi, nil
}
