package service

import (
	"github.com/google/wire"

	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/proto/livekit"
)

var ServiceSet = wire.NewSet(
	NewRoomService,
	NewRTCService,
	NewLivekitServer,
	NewRoomManager,
	wire.Bind(new(livekit.RoomService), new(*RoomService)),
	externalIpFromNode,
)

// helper to construct RTCConfig
func externalIpFromNode(currentNode routing.LocalNode) rtc.ExternalIP {
	return rtc.ExternalIP(currentNode.Ip)
}
