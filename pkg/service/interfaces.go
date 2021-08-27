package service

import (
	"time"

	livekit "github.com/livekit/protocol/proto"

	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc"
)

//go:generate go run github.com/maxbrunsfeld/counterfeiter/v6 -generate

// encapsulates CRUD operations for room settings
// look up participant
//counterfeiter:generate . RoomStore
type RoomStore interface {
	StoreRoom(room *livekit.Room) error
	LoadRoom(idOrName string) (*livekit.Room, error)
	ListRooms() ([]*livekit.Room, error)
	DeleteRoom(idOrName string) error

	// enable locking on a specific room to prevent race
	// returns a (lock uuid, error)
	LockRoom(name string, duration time.Duration) (string, error)
	UnlockRoom(name string, uid string) error

	PersistParticipant(roomName string, participant *livekit.ParticipantInfo) error
	GetParticipant(roomName, identity string) (*livekit.ParticipantInfo, error)
	ListParticipants(roomName string) ([]*livekit.ParticipantInfo, error)
	DeleteParticipant(roomName, identity string) error
}

type RoomManager interface {
	RoomStore

	CreateRoom(req *livekit.CreateRoomRequest) (*livekit.Room, error)
	GetRoom(roomName string) *rtc.Room
	DeleteRoom(roomName string) error
	CleanupRooms() error
	CloseIdleRooms()
	Stop()
	StartSession(roomName string, pi routing.ParticipantInit, requestSource routing.MessageSource, responseSink routing.MessageSink)
}
