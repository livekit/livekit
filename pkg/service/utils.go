package service

import (
	"net/http"

	"github.com/google/wire"
	"go.uber.org/zap"

	"github.com/livekit/livekit-server/pkg/logger"
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

func handleError(w http.ResponseWriter, status int, msg string) {
	l := logger.Desugar().WithOptions(zap.AddCallerSkip(1))
	l.Debug("error handling request", zap.String("error", msg), zap.Int("status", status))
	w.WriteHeader(status)
	w.Write([]byte(msg))
}
