package service

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gorilla/websocket"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc"
	livekit "github.com/livekit/livekit-server/proto"
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
	// reject non websocket requests
	if !websocket.IsWebSocketUpgrade(r) {
		w.WriteHeader(404)
		return
	}

	roomName := r.FormValue("room")
	reconnectParam := r.FormValue("reconnect")

	claims := GetGrants(r.Context())
	// require a claim
	if claims == nil || claims.Video == nil {
		handleError(w, http.StatusUnauthorized, rtc.ErrPermissionDenied.Error())
		return
	}
	pi := routing.ParticipantInit{
		Reconnect: boolValue(reconnectParam),
		Identity:  claims.Identity,
	}
	// only use permissions if any of them are set, default permissive
	if claims.Video.CanPublish || claims.Video.CanSubscribe {
		pi.Permission = &livekit.ParticipantPermission{
			CanSubscribe: claims.Video.CanSubscribe,
			CanPublish:   claims.Video.CanPublish,
		}
	}

	onlyName, err := EnsureJoinPermission(r.Context())
	if err != nil {
		handleError(w, http.StatusUnauthorized, err.Error())
		return
	}
	if onlyName != "" {
		roomName = onlyName
	}

	// create room if it doesn't exist, also assigns an RTC node for the room
	rm, err := s.roomManager.CreateRoom(&livekit.CreateRoomRequest{Name: roomName})
	if err != nil {
		handleError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if claims.Metadata != "" {
		pi.Metadata = claims.Metadata
	}

	// this needs to be started first *before* using router functions on this node
	connId, reqSink, resSource, err := s.router.StartParticipantSignal(roomName, pi)
	if err != nil {
		handleError(w, http.StatusInternalServerError, "could not start session: "+err.Error())
		return
	}

	done := make(chan bool, 1)
	// function exits when websocket terminates, it'll close the event reading off of response sink as well
	defer func() {
		logger.Infow("WS connection closed", "participant", pi.Identity, "connectionId", connId)
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
		"connectionId", connId,
		"room", rm.Sid,
		"roomName", rm.Name,
		"name", pi.Identity,
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
					logger.Infow("source closed connection",
						"participant", pi.Identity,
						"connectionId", connId)
					return
				}
				res, ok := msg.(*livekit.SignalResponse)
				if !ok {
					logger.Errorw("unexpected message type",
						"type", fmt.Sprintf("%T", msg),
						"participant", pi.Identity,
						"connectionId", connId)
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
		if err != nil {
			if err == io.EOF || strings.HasSuffix(err.Error(), "use of closed network connection") ||
				websocket.IsCloseError(err, websocket.CloseAbnormalClosure, websocket.CloseGoingAway, websocket.CloseNormalClosure, websocket.CloseNoStatusReceived) {
				return
			} else {
				logger.Errorw("error reading from websocket", "error", err)
				return
			}
		}
		if err := reqSink.WriteMessage(req); err != nil {
			logger.Warnw("error writing to request sink",
				"error", err,
				"participant", pi.Identity,
				"connectionId", connId)
		}
	}
}
