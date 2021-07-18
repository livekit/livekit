package service

import (
	"net/http"

	"github.com/google/wire"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/logger"
	livekit "github.com/livekit/livekit-server/proto"
)

var ServiceSet = wire.NewSet(
	NewRoomService,
	NewRTCService,
	NewLivekitServer,
	NewRoomManager,
	NewTurnServer,
	config.GetAudioConfig,
	wire.Bind(new(livekit.RoomService), new(*RoomService)),
)

func handleError(w http.ResponseWriter, status int, msg string) {
	// GetLogger already with extra depth 1
	logger.GetLogger().V(1).Info("error handling request", "error", msg, "status", status)
	w.WriteHeader(status)
	_, _ = w.Write([]byte(msg))
}

func boolValue(s string) bool {
	return s == "1" || s == "true"
}
