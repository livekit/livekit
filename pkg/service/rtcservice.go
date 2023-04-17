package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/ua-parser/uap-go/uaparser"

	"github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/routing/selector"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/pkg/telemetry"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
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
	telemetry     telemetry.TelemetryService
}

func NewRTCService(
	conf *config.Config,
	ra RoomAllocator,
	store ServiceStore,
	router routing.MessageRouter,
	currentNode routing.LocalNode,
	telemetry telemetry.TelemetryService,
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
		telemetry:     telemetry,
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
	var pi routing.ParticipantInit

	// require a claim
	if claims == nil || claims.Video == nil {
		return "", pi, http.StatusUnauthorized, rtc.ErrPermissionDenied
	}

	onlyName, err := EnsureJoinPermission(r.Context())
	if err != nil {
		return "", pi, http.StatusUnauthorized, err
	}

	if claims.Identity == "" {
		return "", pi, http.StatusBadRequest, ErrIdentityEmpty
	}

	roomName := livekit.RoomName(r.FormValue("room"))
	reconnectParam := r.FormValue("reconnect")
	reconnectReason, _ := strconv.Atoi(r.FormValue("reconnect_reason")) // 0 means unknown reason
	autoSubParam := r.FormValue("auto_subscribe")
	publishParam := r.FormValue("publish")
	adaptiveStreamParam := r.FormValue("adaptive_stream")
	participantID := r.FormValue("sid")
	subscriberAllowPauseParam := r.FormValue("subscriber_allow_pause")

	if onlyName != "" {
		roomName = onlyName
	}

	// this is new connection for existing participant -  with publish only permissions
	if publishParam != "" {
		// Make sure grant has GetCanPublish set,
		if !claims.Video.GetCanPublish() {
			return "", routing.ParticipantInit{}, http.StatusUnauthorized, rtc.ErrPermissionDenied
		}
		// Make sure by default subscribe is off
		claims.Video.SetCanSubscribe(false)
		claims.Identity += "#" + publishParam
	}

	// room allocator validations
	err = s.roomAllocator.ValidateCreateRoom(r.Context(), roomName)
	if err != nil {
		if errors.Is(err, ErrRoomNotFound) {
			return "", pi, http.StatusNotFound, err
		} else {
			return "", pi, http.StatusInternalServerError, err
		}
	}

	region := ""
	if router, ok := s.router.(routing.Router); ok {
		region = router.GetRegion()
		if foundNode, err := router.GetNodeForRoom(r.Context(), roomName); err == nil {
			if selector.LimitsReached(s.limits, foundNode.Stats) {
				return "", pi, http.StatusServiceUnavailable, rtc.ErrLimitExceeded
			}
		}
	}

	pi = routing.ParticipantInit{
		Reconnect:       boolValue(reconnectParam),
		ReconnectReason: livekit.ReconnectReason(reconnectReason),
		Identity:        livekit.ParticipantIdentity(claims.Identity),
		Name:            livekit.ParticipantName(claims.Name),
		AutoSubscribe:   true,
		Client:          s.ParseClientInfo(r),
		Grants:          claims,
		Region:          region,
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
	if subscriberAllowPauseParam != "" {
		subscriberAllowPause := boolValue(subscriberAllowPauseParam)
		pi.SubscriberAllowPause = &subscriberAllowPause
	}

	return roomName, pi, http.StatusOK, nil
}

func (s *RTCService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// reject non websocket requests
	if !websocket.IsWebSocketUpgrade(r) {
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

	// give it a few attempts to start session
	var cr connectionResult
	var initialResponse *livekit.SignalResponse
	for i := 0; i < 3; i++ {
		connectionTimeout := 3 * time.Second * time.Duration(i+1)
		ctx := utils.ContextWithAttempt(r.Context(), i)
		cr, initialResponse, err = s.startConnection(ctx, roomName, pi, connectionTimeout)
		if err == nil {
			break
		}
		if i < 2 {
			fieldsWithAttempt := append(loggerFields, "attempt", i)
			logger.Warnw("failed to start connection, retrying", err, fieldsWithAttempt...)
		}
	}
	if err != nil {
		prometheus.IncrementParticipantJoinFail(1)
		handleError(w, http.StatusInternalServerError, err, loggerFields...)
		return
	}

	prometheus.IncrementParticipantJoin(1)

	if !pi.Reconnect && initialResponse.GetJoin() != nil {
		pi.ID = livekit.ParticipantID(initialResponse.GetJoin().GetParticipant().GetSid())
	}

	var signalStats *telemetry.BytesTrackStats
	if pi.ID != "" {
		signalStats = telemetry.NewBytesTrackStats(
			telemetry.BytesTrackIDForParticipantID(telemetry.BytesTrackTypeSignal, pi.ID),
			pi.ID,
			s.telemetry)
	}

	pLogger := rtc.LoggerWithParticipant(
		rtc.LoggerWithRoom(logger.GetLogger(), roomName, livekit.RoomID(cr.Room.Sid)),
		pi.Identity,
		pi.ID,
		false,
	)

	done := make(chan struct{})
	// function exits when websocket terminates, it'll close the event reading off of response sink as well
	defer func() {
		pLogger.Infow("finishing WS connection", "connID", cr.ConnectionID)
		cr.ResponseSource.Close()
		cr.RequestSink.Close()
		close(done)

		if signalStats != nil {
			signalStats.Report()
		}
	}()

	// upgrade only once the basics are good to go
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		handleError(w, http.StatusInternalServerError, err, loggerFields...)
		return
	}

	// websocket established
	sigConn := NewWSSignalConnection(conn)
	if count, err := sigConn.WriteResponse(initialResponse); err != nil {
		pLogger.Warnw("could not write initial response", err)
		return
	} else {
		if signalStats != nil {
			signalStats.AddBytes(uint64(count), true)
		}
	}
	pLogger.Infow("new client WS connected",
		"connID", cr.ConnectionID,
		"reconnect", pi.Reconnect,
		"reconnectReason", pi.ReconnectReason,
		"adaptiveStream", pi.AdaptiveStream,
	)

	// handle responses
	go func() {
		defer func() {
			// when the source is terminated, this means Participant.Close had been called and RTC connection is done
			// we would terminate the signal connection as well
			_ = conn.Close()
		}()
		defer func() {
			if r := rtc.Recover(pLogger); r != nil {
				os.Exit(1)
			}
		}()
		for {
			select {
			case <-done:
				return
			case msg := <-cr.ResponseSource.ReadChan():
				if msg == nil {
					return
				}
				res, ok := msg.(*livekit.SignalResponse)
				if !ok {
					pLogger.Errorw("unexpected message type", nil,
						"type", fmt.Sprintf("%T", msg),
						"connID", cr.ConnectionID)
					continue
				}

				switch m := res.Message.(type) {
				case *livekit.SignalResponse_Offer:
					pLogger.Debugw("sending offer", "offer", m)
				case *livekit.SignalResponse_Answer:
					pLogger.Debugw("sending answer", "answer", m)
				}

				if count, err := sigConn.WriteResponse(res); err != nil {
					pLogger.Warnw("error writing to websocket", err)
					return
				} else if signalStats != nil {
					signalStats.AddBytes(uint64(count), true)
				}
			}
		}
	}()

	// handle incoming requests from websocket
	for {
		req, count, err := sigConn.ReadRequest()
		// normal closure
		if err != nil {
			if err == io.EOF || strings.HasSuffix(err.Error(), "use of closed network connection") ||
				websocket.IsCloseError(err, websocket.CloseAbnormalClosure, websocket.CloseGoingAway, websocket.CloseNormalClosure, websocket.CloseNoStatusReceived) {
				pLogger.Debugw("exit ws read loop for closed connection", "connID", cr.ConnectionID)
				return
			} else {
				pLogger.Errorw("error reading from websocket", err)
				return
			}
		}
		if signalStats != nil {
			signalStats.AddBytes(uint64(count), false)
		}

		switch m := req.Message.(type) {
		case *livekit.SignalRequest_Ping:
			count, perr := sigConn.WriteResponse(&livekit.SignalResponse{
				Message: &livekit.SignalResponse_Pong{
					//
					// Although this field is int64, some clients (like JS) cause overflow if nanosecond granularity is used.
					// So. use UnixMillis().
					//
					Pong: time.Now().UnixMilli(),
				},
			})
			if perr == nil && signalStats != nil {
				signalStats.AddBytes(uint64(count), true)
			}
		case *livekit.SignalRequest_PingReq:
			count, perr := sigConn.WriteResponse(&livekit.SignalResponse{
				Message: &livekit.SignalResponse_PongResp{
					PongResp: &livekit.Pong{
						LastPingTimestamp: m.PingReq.Timestamp,
						Timestamp:         time.Now().UnixMilli(),
					},
				},
			})
			if perr == nil && signalStats != nil {
				signalStats.AddBytes(uint64(count), true)
			}
		}

		switch m := req.Message.(type) {
		case *livekit.SignalRequest_Offer:
			pLogger.Debugw("received offer", "offer", m)
		case *livekit.SignalRequest_Answer:
			pLogger.Debugw("received answer", "answer", m)
		}

		if err := cr.RequestSink.WriteMessage(req); err != nil {
			pLogger.Warnw("error writing to request sink", err,
				"connID", cr.ConnectionID)
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
	case "reactnative":
		ci.Sdk = livekit.ClientInfo_REACT_NATIVE
	case "rust":
		ci.Sdk = livekit.ClientInfo_RUST
	}

	ci.Version = values.Get("version")
	ci.Os = values.Get("os")
	ci.OsVersion = values.Get("os_version")
	ci.Browser = values.Get("browser")
	ci.BrowserVersion = values.Get("browser_version")
	ci.DeviceModel = values.Get("device_model")
	ci.Network = values.Get("network")
	// get real address (forwarded http header) - check Cloudflare headers first, fall back to X-Forwarded-For
	ci.Address = GetClientIP(r)

	// attempt to parse types for SDKs that support browser as a platform
	if ci.Sdk == livekit.ClientInfo_JS ||
		ci.Sdk == livekit.ClientInfo_REACT_NATIVE ||
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

type connectionResult struct {
	Room           *livekit.Room
	ConnectionID   livekit.ConnectionID
	RequestSink    routing.MessageSink
	ResponseSource routing.MessageSource
}

func (s *RTCService) startConnection(ctx context.Context, roomName livekit.RoomName, pi routing.ParticipantInit, timeout time.Duration) (connectionResult, *livekit.SignalResponse, error) {
	var cr connectionResult
	var err error
	cr.Room, err = s.roomAllocator.CreateRoom(ctx, &livekit.CreateRoomRequest{Name: string(roomName)})
	if err != nil {
		return cr, nil, err
	}

	// this needs to be started first *before* using router functions on this node
	cr.ConnectionID, cr.RequestSink, cr.ResponseSource, err = s.router.StartParticipantSignal(ctx, roomName, pi)
	if err != nil {
		return cr, nil, err
	}

	// wait for the first message before upgrading to websocket. If no one is
	// responding to our connection attempt, we should terminate the connection
	// instead of waiting forever on the WebSocket
	initialResponse, err := readInitialResponse(cr.ResponseSource, timeout)
	if err != nil {
		// close the connection to avoid leaking
		cr.RequestSink.Close()
		cr.ResponseSource.Close()
		return cr, nil, err
	}
	return cr, initialResponse, nil
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
