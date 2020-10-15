package rtc

import (
	"sync"

	"github.com/pion/webrtc/v3"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/proto/livekit"
)

type RoomManager struct {
	rtcConf    config.RTCConfig
	externalIP string

	feedbackTypes []webrtc.RTCPFeedback

	rooms    map[string]*Room
	roomLock sync.Mutex
}

func NewRoomManager(rtcConf config.RTCConfig, externalIP string) (m *RoomManager, err error) {
	m = &RoomManager{
		rtcConf:    rtcConf,
		externalIP: externalIP,
		feedbackTypes: []webrtc.RTCPFeedback{
			{Type: webrtc.TypeRTCPFBCCM},
			{Type: webrtc.TypeRTCPFBNACK},
			{Type: "nack pli"},
		},
		rooms:    make(map[string]*Room),
		roomLock: sync.Mutex{},
	}
	return
}

func (m *RoomManager) GetRoom(roomId string) *Room {
	m.roomLock.Lock()
	defer m.roomLock.Unlock()
	return m.rooms[roomId]
}

func (m *RoomManager) CreateRoom(req *livekit.CreateRoomRequest) (r *Room, err error) {
	r, err = NewRoomForRequest(req)
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
