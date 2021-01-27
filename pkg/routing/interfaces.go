package routing

import (
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/proto/livekit"
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
	// source exposes a one way channel to make it easier to use with select
	ReadChan() <-chan proto.Message
}

type ParticipantCallback func(roomId, participantId, participantName string, requestSource MessageSource, responseSink MessageSink)

// Router allows multiple nodes to coordinate the participant session
//counterfeiter:generate . Router
type Router interface {
	GetNodeForRoom(roomName string) (string, error)
	SetNodeForRoom(roomName string, nodeId string) error
	ClearRoomState(roomName string) error
	RegisterNode() error
	UnregisterNode() error
	GetNode(nodeId string) (*livekit.Node, error)
	ListNodes() ([]*livekit.Node, error)

	// functions for websocket handler
	GetRequestSink(participantId string) (MessageSink, error)
	GetResponseSource(participantId string) (MessageSource, error)
	// participant signal connection is ready to start
	StartParticipantSignal(roomName, participantId, participantName string) error

	// when a new participant's RTC connection is ready to start
	OnNewParticipantRTC(callback ParticipantCallback)
	Start() error
	Stop()
}

// NodeSelector selects an appropriate node to run the current session
//counterfeiter:generate . NodeSelector
type NodeSelector interface {
	SelectNode(nodes []*livekit.Node, room *livekit.Room) (*livekit.Node, error)
}
