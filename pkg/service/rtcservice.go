package service

import (
	"fmt"
	"io"
	"net/http"

	"github.com/gorilla/websocket"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/proto/livekit"
)

type RTCService struct {
	router      routing.Router
	roomManager *RoomManager
	upgrader    websocket.Upgrader
	currentNode routing.LocalNode
	isDev       bool
}

func NewRTCService(conf *config.Config, roomStore RoomStore, roomManager *RoomManager, router routing.Router, currentNode routing.LocalNode) *RTCService {
	s := &RTCService{
		router:      router,
		roomManager: roomManager,
		upgrader:    websocket.Upgrader{
			// increase buffer size to avoid errors such as
			// read: connection reset by peer
			//ReadBufferSize:  10240,
			//WriteBufferSize: 10240,
		},
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
	reconnectParam := r.FormValue("reconnect")
	isReconnect := reconnectParam == "1" || reconnectParam == "true"
	claims := GetGrants(r.Context())
	// require a claim
	if claims == nil || claims.Video == nil {
		handleError(w, http.StatusUnauthorized, rtc.ErrPermissionDenied.Error())
	}
	identity := claims.Identity

	onlyName, err := EnsureJoinPermission(r.Context())
	if err != nil {
		handleError(w, http.StatusUnauthorized, err.Error())
		return
	}
	if onlyName != "" {
		roomName = onlyName
	}

	rm, err := s.roomManager.roomStore.GetRoom(roomName)
	if err == ErrRoomNotFound {
		rm, err = s.roomManager.CreateRoom(&livekit.CreateRoomRequest{Name: roomName})
	} else if err != nil {
		handleError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// this needs to be started first *before* using router functions on this node
	reqSink, resSource, err := s.router.StartParticipantSignal(roomName, identity, isReconnect)
	if err != nil {
		handleError(w, http.StatusInternalServerError, "could not start session: "+err.Error())
		return
	}

	done := make(chan bool, 1)
	defer func() {
		logger.Infow("WS connection closed", "participant", identity)
		reqSink.Close()
		close(done)
	}()

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

	logger.Infow("new client WS connected",
		"room", rm.Sid,
		"roomName", rm.Name,
		"name", identity,
	)

	// handle responses
	go func() {
		defer func() {
			// when the source is terminated, this means Participant.Close had been called and RTC connection is done
			// we would terminate the signal connection as well
			conn.Close()
		}()
		defer rtc.Recover()
		for {
			select {
			case <-done:
				return
			case msg := <-resSource.ReadChan():
				if msg == nil {
					logger.Errorw("source closed connection", "participant", identity)
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
		}
	}()

	// handle incoming requests from websocket
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
