package rtc

import (
	"sync"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/proto/livekit"
)

// A RoomManager maintains active rooms that are hosted on the current node
type RoomManager struct {
	rtcConf    config.RTCConfig
	externalIP string

	config WebRTCConfig

	rooms       map[string]*Room
	roomsByName map[string]*Room
	roomLock    sync.RWMutex
}

func NewRoomManager(rtcConf config.RTCConfig, externalIP string) (m *RoomManager, err error) {
	m = &RoomManager{
		rtcConf:     rtcConf,
		externalIP:  externalIP,
		rooms:       make(map[string]*Room),
		roomsByName: make(map[string]*Room),
		roomLock:    sync.RWMutex{},
	}

	wc, err := NewWebRTCConfig(&rtcConf, externalIP)
	if err != nil {
		return
	}
	m.config = *wc
	return
}

func (m *RoomManager) GetRoom(idOrName string) *Room {
	m.roomLock.RLock()
	defer m.roomLock.RUnlock()
	rm := m.rooms[idOrName]
	if rm == nil {
		rm = m.roomsByName[idOrName]
	}
	return rm
}

func (m *RoomManager) GetRoomWithConstraint(idOrName string, onlyName string) (*Room, error) {
	if idOrName == "" {
		idOrName = onlyName
	}
	if idOrName == "" {
		return nil, ErrRoomIdMissing
	}

	rm := m.GetRoom(idOrName)
	if rm == nil {
		return nil, ErrRoomNotFound
	}

	if onlyName != "" && rm.Name != onlyName {
		return nil, ErrPermissionDenied
	}
	return rm, nil
}

func (m *RoomManager) CreateRoom(req *livekit.CreateRoomRequest) (r *Room, err error) {
	r = NewRoomForRequest(req, &m.config)
	m.roomLock.Lock()
	defer m.roomLock.Unlock()
	m.rooms[r.Sid] = r
	m.roomsByName[r.Name] = r
	return
}

func (m *RoomManager) DeleteRoom(idOrName string) error {
	rm := m.GetRoom(idOrName)
	if rm == nil {
		return nil
	}
	m.roomLock.Lock()
	defer m.roomLock.Unlock()
	delete(m.rooms, rm.Sid)
	delete(m.roomsByName, rm.Name)
	return nil
}

func (m *RoomManager) Config() *WebRTCConfig {
	return &m.config
}
