package rtc

import (
	"sync"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/proto/livekit"
)

type RoomManager struct {
	rtcConf    config.RTCConfig
	externalIP string

	config WebRTCConfig

	rooms    map[string]*Room
	roomLock sync.Mutex
}

func NewRoomManager(rtcConf config.RTCConfig, externalIP string) (m *RoomManager, err error) {
	m = &RoomManager{
		rtcConf:    rtcConf,
		externalIP: externalIP,
		rooms:      make(map[string]*Room),
		roomLock:   sync.Mutex{},
	}

	wc, err := NewWebRTCConfig(&rtcConf, externalIP)
	if err != nil {
		return
	}
	m.config = *wc
	return
}

func (m *RoomManager) GetRoom(roomId string) *Room {
	m.roomLock.Lock()
	defer m.roomLock.Unlock()
	return m.rooms[roomId]
}

func (m *RoomManager) CreateRoom(req *livekit.CreateRoomRequest) (r *Room, err error) {
	r, err = NewRoomForRequest(req, &m.config)
	if err != nil {
		return
	}
	m.roomLock.Lock()
	defer m.roomLock.Unlock()
	m.rooms[r.RoomId] = r
	return
}

func (m *RoomManager) DeleteRoom(roomId string) error {
	m.roomLock.Lock()
	defer m.roomLock.Unlock()
	delete(m.rooms, roomId)
	return nil
}
