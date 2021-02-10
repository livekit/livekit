package service

import (
	"github.com/livekit/livekit-server/proto/livekit"
)

//go:generate go run github.com/maxbrunsfeld/counterfeiter/v6 -generate

// encapsulates CRUD operations for room settings
// look up participant
//counterfeiter:generate . RoomStore
type RoomStore interface {
	CreateRoom(room *livekit.Room) error
	GetRoom(idOrName string) (*livekit.Room, error)
	ListRooms() ([]*livekit.Room, error)
	DeleteRoom(idOrName string) error

	PersistParticipant(roomName string, participant *livekit.ParticipantInfo) error
	GetParticipant(roomName, identity string) (*livekit.ParticipantInfo, error)
	ListParticipants(roomName string) ([]*livekit.ParticipantInfo, error)
	DeleteParticipant(roomName, identity string) error
}
