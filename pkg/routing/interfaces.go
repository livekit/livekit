package routing

import (
	"github.com/livekit/livekit-server/proto/livekit"
)

//go:generate go run github.com/maxbrunsfeld/counterfeiter/v6 -generate

// routes signaling message
//counterfeiter:generate . MessageSink
type MessageSink interface {
	WriteMessage(msg interface{}) error
	Close()
}

//counterfeiter:generate . MessageSource
type MessageSource interface {
	ReadMessage() (interface{}, error)
}

type ParticipantCallback func(roomId, participantId, participantName string, requestSource MessageSource, responseSink MessageSink)

//counterfeiter:generate . Router
type Router interface {
	GetNodeIdForRoom(roomName string) (string, error)
	RegisterNode(node *livekit.Node) error
	GetNode(nodeId string) (*livekit.Node, error)

	StartParticipant(roomName, participantId, participantName, nodeId string) error
	SetRTCNode(participantId, nodeId string) error
	// functions for websocket handler
	GetRequestSink(participantId string) MessageSink
	GetResponseSource(participantId string) MessageSource

	OnNewParticipant(callback ParticipantCallback)
	Start() error
	Stop()
}
