package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sebest/xff"
	"github.com/ua-parser/uap-go/uaparser"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/routing/selector"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
)

const (
	maxInitialResponseWait = 10 * time.Second
)

type RTCService struct {
	router        routing.MessageRouter
	roomAllocator RoomAllocator
	store         ServiceStore
	upgrader      websocket.Upgrader
	currentNode   routing.LocalNode
	config        *config.Config
	isDev         bool
	limits        config.LimitConfig
	parser        *uaparser.Parser
}

func NewRTCService(
	conf *config.Config,
	ra RoomAllocator,
	store ServiceStore,
	router routing.MessageRouter,
	currentNode routing.LocalNode,
) *RTCService {
	s := &RTCService{
		router:        router,
		roomAllocator: ra,
		store:         store,
		upgrader:      websocket.Upgrader{},
		currentNode:   currentNode,
		config:        conf,
		isDev:         conf.Development,
		limits:        conf.Limit,
		parser:        uaparser.NewFromSaved(),
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
		handleError(w, code, err)
		return
	}
	_, _ = w.Write([]byte("success"))
}

func (s *RTCService) validate(r *http.Request) (livekit.RoomName, routing.ParticipantInit, int, error) {
	claims := GetGrants(r.Context())
	// require a claim
	if claims == nil || claims.Video == nil {
		return "", routing.ParticipantInit{}, http.StatusUnauthorized, rtc.ErrPermissionDenied
	}

	onlyName, err := EnsureJoinPermission(r.Context())
	if err != nil {
		return "", routing.ParticipantInit{}, http.StatusUnauthorized, err
	}

	if claims.Identity == "" {
		return "", routing.ParticipantInit{}, http.StatusBadRequest, ErrIdentityEmpty
	}

	roomName := livekit.RoomName(r.FormValue("room"))
	reconnectParam := r.FormValue("reconnect")
	autoSubParam := r.FormValue("auto_subscribe")
	publishParam := r.FormValue("publish")
	adaptiveStreamParam := r.FormValue("adaptive_stream")
	participantID := r.FormValue("sid")

	if onlyName != "" {
		roomName = onlyName
	}

	// this is new connection for existing participant -  with publish only permissions
	if publishParam != "" {
		// Make sure grant has CanPublish set,
		if !claims.Video.GetCanPublish() {
			return "", routing.ParticipantInit{}, http.StatusUnauthorized, rtc.ErrPermissionDenied
		}
		// Make sure by default subscribe is off
		claims.Video.SetCanSubscribe(false)
		claims.Identity += "#" + publishParam
	}

	region := ""
	if router, ok := s.router.(routing.Router); ok {
		region = router.GetRegion()
		if foundNode, err := router.GetNodeForRoom(r.Context(), roomName); err == nil {
			if selector.LimitsReached(s.limits, foundNode.Stats) {
				return "", routing.ParticipantInit{}, http.StatusServiceUnavailable, rtc.ErrLimitExceeded
			}
		}
	}

	pi := routing.ParticipantInit{
		Reconnect:     boolValue(reconnectParam),
		Identity:      livekit.ParticipantIdentity(claims.Identity),
		Name:          livekit.ParticipantName(claims.Name),
		AutoSubscribe: true,
		Client:        s.ParseClientInfo(r),
		Grants:        claims,
		Region:        region,
	}
	if pi.Reconnect {
		pi.ID = livekit.ParticipantID(participantID)
	}

	if autoSubParam != "" {
		pi.AutoSubscribe = boolValue(autoSubParam)
	}
	if adaptiveStreamParam != "" {
		pi.AdaptiveStream = boolValue(adaptiveStreamParam)
	}

	return roomName, pi, http.StatusOK, nil
}

func (s *RTCService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// reject non websocket requests
	if !websocket.IsWebSocketUpgrade(r) {
		prometheus.ServiceOperationCounter.WithLabelValues("signal_ws", "error", "reject").Add(1)
		w.WriteHeader(404)
		return
	}

	roomName, pi, code, err := s.validate(r)
	if err != nil {
		handleError(w, code, err)
		return
	}

	// for logger
	loggerFields := []interface{}{
		"participant", pi.Identity,
		"room", roomName,
		"remote", false,
	}

	// when auto create is disabled, we'll check to ensure it's already created
	if !s.config.Room.AutoCreate {
		_, _, err := s.store.LoadRoom(context.Background(), roomName, false)
		if err == ErrRoomNotFound {
			handleError(w, 404, err, loggerFields...)
			return
		} else if err != nil {
			handleError(w, 500, err, loggerFields...)
			return
		}
	}

	// create room if it doesn't exist, also assigns an RTC node for the room
	rm, err := s.roomAllocator.CreateRoom(r.Context(), &livekit.CreateRoomRequest{Name: string(roomName)})
	if err != nil {
		prometheus.ServiceOperationCounter.WithLabelValues("signal_ws", "error", "create_room").Add(1)
		handleError(w, http.StatusInternalServerError, err, loggerFields...)
		return
	}

	// this needs to be started first *before* using router functions on this node
	connId, reqSink, resSource, err := s.router.StartParticipantSignal(r.Context(), roomName, pi)
	if err != nil {
		prometheus.ServiceOperationCounter.WithLabelValues("signal_ws", "error", "start_signal").Add(1)
		handleError(w, http.StatusInternalServerError, err, loggerFields...)
		return
	}

	pLogger := rtc.LoggerWithParticipant(
		rtc.LoggerWithRoom(logger.GetDefaultLogger(), roomName, livekit.RoomID(rm.Sid)),
		pi.Identity,
		"",
		false,
	)

	// wait for the first message before upgrading to websocket. If no one is
	// responding to our connection attempt, we should terminate the connection
	// instead of waiting forever on the WebSocket
	initialResponse, err := readInitialResponse(resSource, maxInitialResponseWait)
	if err != nil {
		prometheus.ServiceOperationCounter.WithLabelValues("signal_ws", "error", "initial_response").Add(1)
		handleError(w, http.StatusInternalServerError, err, loggerFields...)
		return
	}

	done := make(chan struct{})
	// function exits when websocket terminates, it'll close the event reading off of response sink as well
	defer func() {
		pLogger.Infow("finishing WS connection", "connID", connId)
		resSource.Close()
		reqSink.Close()
		close(done)
	}()

	// upgrade only once the basics are good to go
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		prometheus.ServiceOperationCounter.WithLabelValues("signal_ws", "error", "upgrade").Add(1)
		handleError(w, http.StatusInternalServerError, err, loggerFields...)
		return
	}

	// websocket established
	sigConn := NewWSSignalConnection(conn)
	if err := sigConn.WriteResponse(initialResponse); err != nil {
		pLogger.Warnw("could not write initial response", err)
		return
	}

	prometheus.ServiceOperationCounter.WithLabelValues("signal_ws", "success", "").Add(1)
	pLogger.Infow("new client WS connected", "connID", connId)

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
					return
				}
				res, ok := msg.(*livekit.SignalResponse)
				if !ok {
					pLogger.Errorw("unexpected message type", nil,
						"type", fmt.Sprintf("%T", msg),
						"connID", connId)
					continue
				}

				if err = sigConn.WriteResponse(res); err != nil {
					pLogger.Warnw("error writing to websocket", err)
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
				pLogger.Debugw("exit ws read loop for closed connection", "connID", connId)
				return
			} else {
				pLogger.Errorw("error reading from websocket", err)
				return
			}
		}
		if _, ok := req.Message.(*livekit.SignalRequest_Ping); ok {
			_ = sigConn.WriteResponse(&livekit.SignalResponse{
				Message: &livekit.SignalResponse_Pong{
					//
					// Although this field is int64, some clients (like JS) cause overflow if nanosecond granularity is used.
					// So. use UnixMillis().
					//
					Pong: time.Now().UnixMilli(),
				},
			})
			continue
		}
		if err := reqSink.WriteMessage(req); err != nil {
			pLogger.Warnw("error writing to request sink", err,
				"connID", connId)
		}
	}
}

func (s *RTCService) ParseClientInfo(r *http.Request) *livekit.ClientInfo {
	values := r.Form
	ci := &livekit.ClientInfo{}
	if pv, err := strconv.Atoi(values.Get("protocol")); err == nil {
		ci.Protocol = int32(pv)
	}
	sdkString := values.Get("sdk")
	switch sdkString {
	case "js":
		ci.Sdk = livekit.ClientInfo_JS
	case "ios", "swift":
		ci.Sdk = livekit.ClientInfo_SWIFT
	case "android":
		ci.Sdk = livekit.ClientInfo_ANDROID
	case "flutter":
		ci.Sdk = livekit.ClientInfo_FLUTTER
	case "go":
		ci.Sdk = livekit.ClientInfo_GO
	case "unity":
		ci.Sdk = livekit.ClientInfo_UNITY
	}

	ci.Version = values.Get("version")
	ci.Os = values.Get("os")
	ci.OsVersion = values.Get("os_version")
	ci.Browser = values.Get("browser")
	ci.BrowserVersion = values.Get("browser_version")
	ci.DeviceModel = values.Get("device_model")
	ci.Network = values.Get("network")
	// get real address (forwarded http header) - check Cloudflare headers first, fall back to X-Forwarded-For
	ci.Address = r.Header.Get("CF-Connecting-IP")
	if len(ci.Address) == 0 {
		ci.Address = xff.GetRemoteAddr(r)
	}

	// attempt to parse types for SDKs that support browser as a platform
	if ci.Sdk == livekit.ClientInfo_JS ||
		ci.Sdk == livekit.ClientInfo_FLUTTER ||
		ci.Sdk == livekit.ClientInfo_UNITY {
		client := s.parser.Parse(r.UserAgent())
		if ci.Browser == "" {
			ci.Browser = client.UserAgent.Family
			ci.BrowserVersion = client.UserAgent.ToVersionString()
		}
		if ci.Os == "" {
			ci.Os = client.Os.Family
			ci.OsVersion = client.Os.ToVersionString()
		}
		if ci.DeviceModel == "" {
			model := client.Device.Family
			if model != "" && client.Device.Model != "" && model != client.Device.Model {
				model += " " + client.Device.Model
			}

			ci.DeviceModel = model
		}
	}

	return ci
}

func readInitialResponse(source routing.MessageSource, timeout time.Duration) (*livekit.SignalResponse, error) {
	responseTimer := time.NewTimer(timeout)
	defer responseTimer.Stop()
	for {
		select {
		case <-responseTimer.C:
			return nil, errors.New("timed out while waiting for signal response")
		case msg := <-source.ReadChan():
			if msg == nil {
				return nil, errors.New("connection closed by media")
			}
			res, ok := msg.(*livekit.SignalResponse)
			if !ok {
				return nil, fmt.Errorf("unexpected message type: %T", msg)
			}
			return res, nil
		}
	}

}
