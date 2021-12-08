package service

import (
	"context"
	"time"

	livekit "github.com/livekit/protocol/livekit"
)

//go:generate go run github.com/maxbrunsfeld/counterfeiter/v6 -generate

// encapsulates CRUD operations for room settings
//counterfeiter:generate . RoomStore
type RoomStore interface {
	RORoomStore

	// enable locking on a specific room to prevent race
	// returns a (lock uuid, error)
	LockRoom(ctx context.Context, name string, duration time.Duration) (string, error)
	UnlockRoom(ctx context.Context, name string, uid string) error

	StoreRoom(ctx context.Context, room *livekit.Room) error
	DeleteRoom(ctx context.Context, name string) error

	StoreParticipant(ctx context.Context, roomName string, participant *livekit.ParticipantInfo) error
	DeleteParticipant(ctx context.Context, roomName, identity string) error
}

//counterfeiter:generate . RORoomStore
type RORoomStore interface {
	LoadRoom(ctx context.Context, name string) (*livekit.Room, error)
	ListRooms(ctx context.Context) ([]*livekit.Room, error)

	LoadParticipant(ctx context.Context, roomName, identity string) (*livekit.ParticipantInfo, error)
	ListParticipants(ctx context.Context, roomName string) ([]*livekit.ParticipantInfo, error)
}

//counterfeiter:generate . RoomAllocator
type RoomAllocator interface {
	CreateRoom(ctx context.Context, req *livekit.CreateRoomRequest) (*livekit.Room, error)
}
