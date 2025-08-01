package relay

import (
	"github.com/pion/ice/v2"
	"github.com/pion/webrtc/v3"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
)

type RelayConfig struct {
	ID            string
	SettingEngine webrtc.SettingEngine
	ICEServers    []webrtc.ICEServer
	BufferFactory *buffer.Factory
	RelayUDPMux   ice.UDPMux
	RelayPort     uint32
}
