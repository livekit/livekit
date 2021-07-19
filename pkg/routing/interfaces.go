package routing

import (
	"google.golang.org/protobuf/proto"

	livekit "github.com/livekit/livekit-server/proto"
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
	Identity        string
	Metadata        string
	Reconnect       bool
	Permission      *livekit.ParticipantPermission
	ProtocolVersion int32
	UsePlanB        bool
	AutoSubscribe   bool
	Hidden          bool
}

type NewParticipantCallback func(roomName string, pi ParticipantInit, requestSource MessageSource, responseSink MessageSink)
type RTCMessageCallback func(roomName, identity string, msg *livekit.RTCNodeMessage)

// Router allows multiple nodes to coordinate the participant session
//counterfeiter:generate . Router
type Router interface {
	GetNodeForRoom(roomName string) (*livekit.Node, error)
	SetNodeForRoom(roomName string, nodeId string) error
	ClearRoomState(roomName string) error
	RegisterNode() error
	UnregisterNode() error
	RemoveDeadNodes() error
	GetNode(nodeId string) (*livekit.Node, error)
	ListNodes() ([]*livekit.Node, error)

	// StartParticipantSignal participant signal connection is ready to start
	StartParticipantSignal(roomName string, pi ParticipantInit) (connectionId string, reqSink MessageSink, resSource MessageSource, err error)

	// CreateRTCSink sends a message to RTC node
	CreateRTCSink(roomName, identity string) (MessageSink, error)

	// OnNewParticipantRTC is called to start a new participant's RTC connection
	OnNewParticipantRTC(callback NewParticipantCallback)

	// OnRTCMessage is called to execute actions on the RTC node
	OnRTCMessage(callback RTCMessageCallback)

	Start() error
	Stop()
}

// NodeSelector selects an appropriate node to run the current session
//counterfeiter:generate . NodeSelector
type NodeSelector interface {
	SelectNode(nodes []*livekit.Node, room *livekit.Room) (*livekit.Node, error)
}
