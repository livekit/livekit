package service

import (
	"context"
	"time"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/p2p"
)

//go:generate go run github.com/maxbrunsfeld/counterfeiter/v6 -generate

// encapsulates CRUD operations for room settings
//
//counterfeiter:generate . ObjectStore
type ObjectStore interface {
	ServiceStore

	// enable locking on a specific room to prevent race
	// returns a (lock uuid, error)
	LockRoom(ctx context.Context, roomKey livekit.RoomKey, duration time.Duration) (string, error)
	UnlockRoom(ctx context.Context, roomKey livekit.RoomKey, uid string) error

	StoreRoom(ctx context.Context, room *livekit.Room, roomKey livekit.RoomKey, internal *livekit.RoomInternal) error
	DeleteRoom(ctx context.Context, roomKey livekit.RoomKey) error

	StoreParticipant(ctx context.Context, roomKey livekit.RoomKey, participant *livekit.ParticipantInfo) error
	DeleteParticipant(ctx context.Context, roomKey livekit.RoomKey, identity livekit.ParticipantIdentity) error
}

//counterfeiter:generate . ServiceStore
type ServiceStore interface {
	LoadRoom(ctx context.Context, roomKey livekit.RoomKey, includeInternal bool) (*livekit.Room, *livekit.RoomInternal, p2p.RoomCommunicator, error)

	// ListRooms returns currently active rooms. if names is not nil, it'll filter and return
	// only rooms that match
	ListRooms(ctx context.Context, roomKeys []livekit.RoomKey) ([]*livekit.Room, error)
	LoadParticipant(ctx context.Context, roomKey livekit.RoomKey, identity livekit.ParticipantIdentity) (*livekit.ParticipantInfo, error)
	ListParticipants(ctx context.Context, roomKey livekit.RoomKey) ([]*livekit.ParticipantInfo, error)
}

//counterfeiter:generate . EgressStore
type EgressStore interface {
	StoreEgress(ctx context.Context, info *livekit.EgressInfo) error
	LoadEgress(ctx context.Context, egressID string) (*livekit.EgressInfo, error)
	ListEgress(ctx context.Context, roomName livekit.RoomName, active bool) ([]*livekit.EgressInfo, error)
	UpdateEgress(ctx context.Context, info *livekit.EgressInfo) error
}

//counterfeiter:generate . IngressStore
type IngressStore interface {
	StoreIngress(ctx context.Context, info *livekit.IngressInfo) error
	LoadIngress(ctx context.Context, ingressID string) (*livekit.IngressInfo, error)
	LoadIngressFromStreamKey(ctx context.Context, streamKey string) (*livekit.IngressInfo, error)
	ListIngress(ctx context.Context, roomName livekit.RoomName) ([]*livekit.IngressInfo, error)
	UpdateIngress(ctx context.Context, info *livekit.IngressInfo) error
	UpdateIngressState(ctx context.Context, ingressId string, state *livekit.IngressState) error
	DeleteIngress(ctx context.Context, info *livekit.IngressInfo) error
}

//counterfeiter:generate . RoomAllocator
type RoomAllocator interface {
	CreateRoom(ctx context.Context, req *livekit.CreateRoomRequest, apiKey livekit.ApiKey) (*livekit.Room, error)
	ValidateCreateRoom(ctx context.Context, roomKey livekit.RoomKey) error
}
