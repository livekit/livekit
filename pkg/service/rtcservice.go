package service

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/gorilla/websocket"
	"github.com/livekit/protocol/logger"
	livekit "github.com/livekit/protocol/proto"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/utils/stats"
)

type RTCService struct {
	router        routing.Router
	roomAllocator *RoomAllocator
	upgrader      websocket.Upgrader
	currentNode   routing.LocalNode
	isDev         bool
}

func NewRTCService(conf *config.Config, ra *RoomAllocator, router routing.Router, currentNode routing.LocalNode) *RTCService {
	s := &RTCService{
		router:        router,
		roomAllocator: ra,
		upgrader:      websocket.Upgrader{},
		currentNode:   currentNode,
		isDev:         conf.Development,
	}

	// allow connections from any origin, since script may be hosted anywhere
	// security is enforced by access tokens
	s.upgrader.CheckOrigin = func(r *http.Request) bool {
		return true
	}

	return s
}

func (s *RTCService) Validate(w http.ResponseWriter, r *http.Request) {
	_, _, code, err := s.validate(r)
	if err != nil {
		handleError(w, code, err.Error())
		return
	}
	_, _ = w.Write([]byte("success"))
}

func (s *RTCService) validate(r *http.Request) (string, routing.ParticipantInit, int, error) {
	claims := GetGrants(r.Context())
	// require a claim
	if claims == nil || claims.Video == nil {
		return "", routing.ParticipantInit{}, http.StatusUnauthorized, rtc.ErrPermissionDenied
	}

	onlyName, err := EnsureJoinPermission(r.Context())
	if err != nil {
		return "", routing.ParticipantInit{}, http.StatusUnauthorized, err
	}

	roomName := r.FormValue("room")
	reconnectParam := r.FormValue("reconnect")
	autoSubParam := r.FormValue("auto_subscribe")

	if onlyName != "" {
		roomName = onlyName
	}

	pi := routing.ParticipantInit{
		Reconnect:     boolValue(reconnectParam),
		Identity:      claims.Identity,
		AutoSubscribe: true,
		Metadata:      claims.Metadata,
		Hidden:        claims.Video.Hidden,
		Client:        s.parseClientInfo(r.Form),
	}
	if autoSubParam != "" {
		pi.AutoSubscribe = boolValue(autoSubParam)
	}
	pi.Permission = permissionFromGrant(claims.Video)

	return roomName, pi, http.StatusOK, nil
}

func (s *RTCService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// reject non websocket requests
	if !websocket.IsWebSocketUpgrade(r) {
		stats.PromServiceOperationCounter.WithLabelValues("signal_ws", "error", "reject").Add(1)
		w.WriteHeader(404)
		return
	}

	roomName, pi, code, err := s.validate(r)
	if err != nil {
		handleError(w, code, err.Error())
		return
	}

	// create room if it doesn't exist, also assigns an RTC node for the room
	rm, err := s.roomAllocator.CreateRoom(r.Context(), &livekit.CreateRoomRequest{Name: roomName})
	if err != nil {
		stats.PromServiceOperationCounter.WithLabelValues("signal_ws", "error", "create_room").Add(1)
		handleError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// this needs to be started first *before* using router functions on this node
	connId, reqSink, resSource, err := s.router.StartParticipantSignal(r.Context(), roomName, pi)
	if err != nil {
		stats.PromServiceOperationCounter.WithLabelValues("signal_ws", "error", "start_signal").Add(1)
		handleError(w, http.StatusInternalServerError, "could not start session: "+err.Error())
		return
	}

	done := make(chan struct{})
	// function exits when websocket terminates, it'll close the event reading off of response sink as well
	defer func() {
		logger.Infow("server closing WS connection", "participant", pi.Identity, "connID", connId)
		reqSink.Close()
		close(done)
	}()

	// upgrade only once the basics are good to go
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		stats.PromServiceOperationCounter.WithLabelValues("signal_ws", "error", "upgrade").Add(1)
		logger.Warnw("could not upgrade to WS", err)
		handleError(w, http.StatusInternalServerError, err.Error())
		return
	}
	sigConn := NewWSSignalConnection(conn)
	if types.ProtocolVersion(pi.Client.Protocol).SupportsProtobuf() {
		sigConn.useJSON = false
	}

	stats.PromServiceOperationCounter.WithLabelValues("signal_ws", "success", "").Add(1)
	logger.Infow("new client WS connected",
		"connID", connId,
		"roomID", rm.Sid,
		"room", rm.Name,
		"participant", pi.Identity,
	)

	// handle responses
	go func() {
		defer func() {
			// when the source is terminated, this means Participant.Close had been called and RTC connection is done
			// we would terminate the signal connection as well
			_ = conn.Close()
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
						"connID", connId)
					return
				}
				res, ok := msg.(*livekit.SignalResponse)
				if !ok {
					logger.Errorw("unexpected message type", nil,
						"type", fmt.Sprintf("%T", msg),
						"participant", pi.Identity,
						"connID", connId)
					continue
				}

				if err = sigConn.WriteResponse(res); err != nil {
					logger.Warnw("error writing to websocket", err)
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
				logger.Errorw("error reading from websocket", err)
				return
			}
		}
		if err := reqSink.WriteMessage(req); err != nil {
			logger.Warnw("error writing to request sink", err,
				"participant", pi.Identity,
				"connID", connId)
		}
	}
}

func (s *RTCService) parseClientInfo(values url.Values) *livekit.ClientInfo {
	ci := &livekit.ClientInfo{}
	if pv, err := strconv.Atoi(values.Get("protocol")); err == nil {
		ci.Protocol = int32(pv)
	}
	sdkString := values.Get("sdk")
	switch sdkString {
	case "js":
		ci.Sdk = livekit.ClientInfo_JS
	case "ios":
		ci.Sdk = livekit.ClientInfo_IOS
	case "android":
		ci.Sdk = livekit.ClientInfo_ANDROID
	case "flutter":
		ci.Sdk = livekit.ClientInfo_FLUTTER
	case "go":
		ci.Sdk = livekit.ClientInfo_GO
	}
	ci.Version = values.Get("version")
	return ci
}
