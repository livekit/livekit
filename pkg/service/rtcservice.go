package service

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/gorilla/websocket"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/livekit-server/proto/livekit"
)

type RTCService struct {
	router      routing.Router
	roomStore   RoomStore
	upgrader    websocket.Upgrader
	currentNode routing.LocalNode
	isDev       bool
}

func NewRTCService(conf *config.Config, roomStore RoomStore, router routing.Router, currentNode routing.LocalNode) *RTCService {
	s := &RTCService{
		router:      router,
		roomStore:   roomStore,
		upgrader:    websocket.Upgrader{},
		currentNode: currentNode,
		isDev:       conf.Development,
	}

	// allow connections from any origin, since script may be hosted anywhere
	// security is enforced by access tokens
	s.upgrader.CheckOrigin = func(r *http.Request) bool {
		return true
	}

	return s
}

func (s *RTCService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	roomName := r.FormValue("room")
	claims := GetGrants(r.Context())
	// require a claim
	if claims == nil || claims.Video == nil {
		handleError(w, http.StatusUnauthorized, rtc.ErrPermissionDenied.Error())
	}
	pName := claims.Identity

	onlyName, err := EnsureJoinPermission(r.Context())
	if err != nil {
		handleError(w, http.StatusUnauthorized, err.Error())
		return
	}
	if onlyName != "" {
		roomName = onlyName
	}

	rm, err := s.roomStore.GetRoom(roomName)
	if err != nil {
		handleError(w, http.StatusNotFound, err.Error())
		return
	}

	// upgrade only once the basics are good to go
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		logger.Warnw("could not upgrade to WS",
			"err", err,
		)
		handleError(w, http.StatusInternalServerError, err.Error())
		return
	}

	sigConn := NewWSSignalConnection(conn)

	participantId := utils.NewGuid(utils.ParticipantPrefix)
	err = s.router.StartParticipant(roomName, participantId, pName, s.currentNode.Id)
	if err != nil {
		handleError(w, http.StatusInternalServerError, "could not set signal node: "+err.Error())
	}

	logger.Infow("new client connected",
		"room", rm.Sid,
		"roomName", rm.Name,
		"name", pName,
	)

	reqSink := s.router.GetRequestSink(participantId)
	resSource := s.router.GetResponseSource(participantId)

	go func() {
		for {
			msg, err := resSource.ReadMessage()
			if err == io.EOF {
				return
			}
			res, ok := msg.(*livekit.SignalResponse)
			if !ok {
				logger.Errorw("unexpected message type", "type", fmt.Sprintf("%T", msg))
				continue
			}

			if err = sigConn.WriteResponse(res); err != nil {
				logger.Warnw("error writing to websocket", "error", err)
				return
			}
		}
	}()

	defer func() {
		logger.Infow("WS connection closed", "participant", pName)
		reqSink.Close()
	}()
	for {
		req, err := sigConn.ReadRequest()
		// normal closure
		if err == io.EOF || websocket.IsCloseError(err, websocket.CloseAbnormalClosure, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
			return
		} else if err != nil {
			logger.Errorw("error reading from websocket", "error", err)
			return
		}

		if err = reqSink.WriteMessage(req); err != nil {
			logger.Warnw("error writing to request sink", "error", err)
		}
	}
}

type errStruct struct {
	StatusCode int    `json:"statusCode"`
	Error      string `json:"error"`
	Message    string `json:"message,omitempty"`
}

func writeJSONError(w http.ResponseWriter, code int, error ...string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)

	json.NewEncoder(w).Encode(jsonError(code, error...))
}

func jsonError(code int, error ...string) errStruct {
	es := errStruct{
		StatusCode: code,
	}
	if len(error) > 0 {
		es.Error = error[0]
	}
	if len(error) > 1 {
		es.Message = error[1]
	}
	return es
}
