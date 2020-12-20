package rtc

import (
	"github.com/livekit/livekit-server/proto/livekit"
)

// PublishedTrack is the main interface representing a track published to the room
// it's responsible for managing subscribers and forwarding data from the input track to all subscribers
type PublishedTrack interface {
	Start()
	ID() string
	Kind() livekit.TrackInfo_Type
	StreamID() string
	AddSubscriber(participant *Participant) error
	RemoveSubscriber(participantId string)
	RemoveAllSubscribers()
}

func TrackToProto(t PublishedTrack) *livekit.TrackInfo {
	return &livekit.TrackInfo{
		Sid:  t.ID(),
		Type: t.Kind(),
		Name: t.StreamID(),
	}
}
