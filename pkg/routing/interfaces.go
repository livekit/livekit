package routing

import (
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/proto/livekit"
)

//go:generate go run github.com/maxbrunsfeld/counterfeiter/v6 -generate

// routes signaling message
//counterfeiter:generate . MessageSink
type MessageSink interface {
	WriteMessage(msg proto.Message) error
	Close()
	OnClose(f func())
}

//counterfeiter:generate . MessageSource
type MessageSource interface {
	ReadMessage() (proto.Message, error)
}

type ParticipantCallback func(roomId, participantId, participantName string, requestSource MessageSource, responseSink MessageSink)

//counterfeiter:generate . Router
type Router interface {
	GetNodeForRoom(roomName string) (string, error)
	SetNodeForRoom(roomName string, nodeId string) error
	RegisterNode() error
	UnregisterNode() error
	GetNode(nodeId string) (*livekit.Node, error)
	ListNodes() ([]*livekit.Node, error)

	SetParticipantRTCNode(participantId, nodeId string) error

	// functions for websocket handler
	GetRequestSink(participantId string) (MessageSink, error)
	GetResponseSource(participantId string) (MessageSource, error)
	StartParticipant(roomName, participantId, participantName string) error

	OnNewParticipant(callback ParticipantCallback)
	Start() error
	Stop()
}
