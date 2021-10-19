package service

import (
	"context"
	"time"

	livekit "github.com/livekit/protocol/proto"

	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc"
)

//go:generate go run github.com/maxbrunsfeld/counterfeiter/v6 -generate

// encapsulates CRUD operations for room settings
//counterfeiter:generate . RoomStore
type RoomStore interface {
	StoreRoom(ctx context.Context, room *livekit.Room) error
	LoadRoom(ctx context.Context, idOrName string) (*livekit.Room, error)
	ListRooms(ctx context.Context) ([]*livekit.Room, error)
	DeleteRoom(ctx context.Context, idOrName string) error

	// enable locking on a specific room to prevent race
	// returns a (lock uuid, error)
	LockRoom(ctx context.Context, name string, duration time.Duration) (string, error)
	UnlockRoom(ctx context.Context, name string, uid string) error

	StoreParticipant(ctx context.Context, roomName string, participant *livekit.ParticipantInfo) error
	LoadParticipant(ctx context.Context, roomName, identity string) (*livekit.ParticipantInfo, error)
	ListParticipants(ctx context.Context, roomName string) ([]*livekit.ParticipantInfo, error)
	DeleteParticipant(ctx context.Context, roomName, identity string) error
}

type RoomManager interface {
	RoomStore

	GetRoom(ctx context.Context, roomName string) *rtc.Room
	DeleteRoom(ctx context.Context, roomName string) error
	StartSession(ctx context.Context, roomName string, pi routing.ParticipantInit, requestSource routing.MessageSource, responseSink routing.MessageSink)
	CleanupRooms() error
	CloseIdleRooms()
	HasParticipants() bool
	Stop()
}
