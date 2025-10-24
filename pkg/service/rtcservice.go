// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package service

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/atomic"
	"golang.org/x/exp/maps"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/psrpc"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/pkg/telemetry"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/livekit-server/pkg/utils"
)

type RTCService struct {
	router        routing.MessageRouter
	roomAllocator RoomAllocator
	upgrader      websocket.Upgrader
	config        *config.Config
	isDev         bool
	limits        config.LimitConfig
	telemetry     telemetry.TelemetryService

	mu          sync.Mutex
	connections map[*websocket.Conn]struct{}
}

func NewRTCService(
	conf *config.Config,
	ra RoomAllocator,
	router routing.MessageRouter,
	telemetry telemetry.TelemetryService,
) *RTCService {
	s := &RTCService{
		router:        router,
		roomAllocator: ra,
		config:        conf,
		isDev:         conf.Development,
		limits:        conf.Limit,
		telemetry:     telemetry,
		connections:   map[*websocket.Conn]struct{}{},
	}

	s.upgrader = websocket.Upgrader{
		EnableCompression: true,

		// allow connections from any origin, since script may be hosted anywhere
		// security is enforced by access tokens
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}

	return s
}

func (s *RTCService) SetupRoutes(mux *http.ServeMux) {
	mux.Handle("/rtc", s)
	mux.HandleFunc("/rtc/validate", s.validate)
}

func (s *RTCService) validate(w http.ResponseWriter, r *http.Request) {
	lgr := utils.GetLogger(r.Context())
	_, _, code, err := s.validateInternal(lgr, r, true)
	if err != nil {
		HandleError(w, r, code, err)
		return
	}
	_, _ = w.Write([]byte("success"))
}

func decodeAttributes(str string) (map[string]string, error) {
	data, err := base64.URLEncoding.DecodeString(str)
	if err != nil {
		return nil, err
	}
	var attrs map[string]string
	if err := json.Unmarshal(data, &attrs); err != nil {
		return nil, err
	}
	return attrs, nil
}

var gzipReaderPool = sync.Pool{
	New: func() any { return &gzip.Reader{} },
}

func (s *RTCService) validateInternal(lgr logger.Logger, r *http.Request, strict bool) (livekit.RoomName, routing.ParticipantInit, int, error) {
	var params ValidateConnectRequestParams
	useSinglePeerConnection := false
	joinRequest := &livekit.JoinRequest{}

	wrappedJoinRequestBase64 := r.FormValue("join_request")
	if wrappedJoinRequestBase64 == "" {
		params.publish = r.FormValue("publish")

		attributesStrParam := r.FormValue("attributes")
		if attributesStrParam != "" {
			attrs, err := decodeAttributes(attributesStrParam)
			if err != nil {
				if strict {
					return "", routing.ParticipantInit{}, http.StatusBadRequest, errors.New("cannot decode attributes")
				}
				lgr.Debugw("failed to decode attributes", "error", err)
				// attrs will be empty here, so just proceed
			}
			params.attributes = attrs
		}
	} else {
		useSinglePeerConnection = true
		if wrappedProtoBytes, err := base64.URLEncoding.DecodeString(wrappedJoinRequestBase64); err != nil {
			return "", routing.ParticipantInit{}, http.StatusBadRequest, errors.New("cannot base64 decode wrapped join request")
		} else {
			wrappedJoinRequest := &livekit.WrappedJoinRequest{}
			if err := proto.Unmarshal(wrappedProtoBytes, wrappedJoinRequest); err != nil {
				return "", routing.ParticipantInit{}, http.StatusBadRequest, errors.New("cannot unmarshal wrapped join request")
			}

			switch wrappedJoinRequest.Compression {
			case livekit.WrappedJoinRequest_NONE:
				if err := proto.Unmarshal(wrappedJoinRequest.JoinRequest, joinRequest); err != nil {
					return "", routing.ParticipantInit{}, http.StatusBadRequest, errors.New("cannot unmarshal join request")
				}

			case livekit.WrappedJoinRequest_GZIP:
				reader := gzipReaderPool.Get().(*gzip.Reader)
				defer gzipReaderPool.Put(reader)
				reader.Reset(bytes.NewReader(wrappedJoinRequest.JoinRequest))
				protoBytes, err := io.ReadAll(reader)
				if err != nil {
					return "", routing.ParticipantInit{}, http.StatusBadRequest, errors.New("cannot read decompressed join request")
				}

				if err := proto.Unmarshal(protoBytes, joinRequest); err != nil {
					return "", routing.ParticipantInit{}, http.StatusBadRequest, errors.New("cannot unmarshal join request")
				}
			}

			params.metadata = joinRequest.Metadata
			params.attributes = joinRequest.ParticipantAttributes
		}
	}

	res, code, err := ValidateConnectRequest(
		lgr,
		r,
		s.limits,
		params,
		s.router,
		s.roomAllocator,
	)
	if err != nil {
		return res.roomName, routing.ParticipantInit{}, code, err
	}

	pi := routing.ParticipantInit{
		Identity:                livekit.ParticipantIdentity(res.grants.Identity),
		Name:                    livekit.ParticipantName(res.grants.Name),
		Grants:                  res.grants,
		Region:                  res.region,
		CreateRoom:              res.createRoomRequest,
		UseSinglePeerConnection: useSinglePeerConnection,
	}

	if wrappedJoinRequestBase64 == "" {
		pi.Reconnect = boolValue(r.FormValue("reconnect"))
		pi.Client = ParseClientInfo(r)
		pi.AutoSubscribe = true
		pi.AdaptiveStream = boolValue(r.FormValue("adaptive_stream"))
		pi.DisableICELite = boolValue(r.FormValue("disable_ice_lite"))

		reconnectReason, _ := strconv.Atoi(r.FormValue("reconnect_reason")) // 0 means unknown reason
		pi.ReconnectReason = livekit.ReconnectReason(reconnectReason)

		if pi.Reconnect {
			pi.ID = livekit.ParticipantID(r.FormValue("sid"))
		}

		if autoSubscribe := r.FormValue("auto_subscribe"); autoSubscribe != "" {
			pi.AutoSubscribe = boolValue(autoSubscribe)
		}

		subscriberAllowPauseParam := r.FormValue("subscriber_allow_pause")
		if subscriberAllowPauseParam != "" {
			subscriberAllowPause := boolValue(subscriberAllowPauseParam)
			pi.SubscriberAllowPause = &subscriberAllowPause
		}
	} else {
		lgr.Debugw("processing join request", "joinRequest", logger.Proto(joinRequest))

		AugmentClientInfo(joinRequest.ClientInfo, r)
		pi.Client = joinRequest.ClientInfo

		pi.AutoSubscribe = joinRequest.GetConnectionSettings().GetAutoSubscribe()
		pi.AdaptiveStream = joinRequest.GetConnectionSettings().GetAdaptiveStream()
		pi.DisableICELite = joinRequest.GetConnectionSettings().GetDisableIceLite()

		subscriberAllowPause := joinRequest.GetConnectionSettings().GetSubscriberAllowPause()
		pi.SubscriberAllowPause = &subscriberAllowPause

		pi.AddTrackRequests = joinRequest.AddTrackRequests
		pi.PublisherOffer = joinRequest.PublisherOffer

		pi.Reconnect = joinRequest.Reconnect
		pi.ReconnectReason = joinRequest.ReconnectReason
		pi.ID = livekit.ParticipantID(joinRequest.ParticipantSid)
	}

	return res.roomName, pi, code, err
}

func (s *RTCService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// reject non websocket requests
	if !websocket.IsWebSocketUpgrade(r) {
		w.WriteHeader(404)
		return
	}

	var (
		roomName            livekit.RoomName
		roomID              livekit.RoomID
		participantIdentity livekit.ParticipantIdentity
		pID                 livekit.ParticipantID
		loggerResolved      bool

		pi   routing.ParticipantInit
		code int
		err  error
	)

	pLogger, loggerResolver := utils.GetLogger(r.Context()).WithDeferredValues()

	getLoggerFields := func() []any {
		return []any{
			"room", roomName,
			"roomID", roomID,
			"participant", participantIdentity,
			"pID", pID,
		}
	}

	resolveLogger := func(force bool) {
		if loggerResolved {
			return
		}

		if force || (roomName != "" && roomID != "" && participantIdentity != "" && pID != "") {
			loggerResolved = true
			loggerResolver.Resolve(getLoggerFields()...)
		}
	}

	resetLogger := func() {
		loggerResolver.Reset()

		roomName = ""
		roomID = ""
		participantIdentity = ""
		pID = ""
		loggerResolved = false
	}

	roomName, pi, code, err = s.validateInternal(pLogger, r, false)
	if err != nil {
		HandleError(w, r, code, err)
		return
	}

	participantIdentity = pi.Identity
	if pi.ID != "" {
		pID = pi.ID
	}

	// give it a few attempts to start session
	var cr connectionResult
	var initialResponse *livekit.SignalResponse
	for attempt := 0; attempt < s.config.SignalRelay.ConnectAttempts; attempt++ {
		connectionTimeout := 3 * time.Second * time.Duration(attempt+1)
		ctx := utils.ContextWithAttempt(r.Context(), attempt)
		cr, initialResponse, err = s.startConnection(ctx, roomName, pi, connectionTimeout)
		if err == nil || errors.Is(err, context.Canceled) {
			break
		}
	}

	if err != nil {
		prometheus.IncrementParticipantJoinFail(1)
		status := http.StatusInternalServerError
		var psrpcErr psrpc.Error
		if errors.As(err, &psrpcErr) {
			status = psrpcErr.ToHttp()
		}
		HandleError(w, r, status, err, getLoggerFields()...)
		return
	}

	prometheus.IncrementParticipantJoin(1)

	pLogger = pLogger.WithValues("connID", cr.ConnectionID)
	if !pi.Reconnect && initialResponse.GetJoin() != nil {
		joinRoomID := livekit.RoomID(initialResponse.GetJoin().GetRoom().GetSid())
		if joinRoomID != "" {
			roomID = joinRoomID
		}

		pi.ID = livekit.ParticipantID(initialResponse.GetJoin().GetParticipant().GetSid())
		pID = pi.ID

		resolveLogger(false)
	}

	signalStats := telemetry.NewBytesSignalStats(r.Context(), s.telemetry)
	if join := initialResponse.GetJoin(); join != nil {
		signalStats.ResolveRoom(join.GetRoom())
		signalStats.ResolveParticipant(join.GetParticipant())
	}
	if pi.Reconnect && pi.ID != "" {
		signalStats.ResolveParticipant(&livekit.ParticipantInfo{
			Sid:      string(pi.ID),
			Identity: string(pi.Identity),
		})
	}

	closedByClient := atomic.NewBool(false)
	done := make(chan struct{})
	// function exits when websocket terminates, it'll close the event reading off of request sink and response source as well
	defer func() {
		pLogger.Debugw("finishing WS connection", "closedByClient", closedByClient.Load())
		cr.ResponseSource.Close()
		cr.RequestSink.Close()
		close(done)

		signalStats.Stop()
	}()

	// upgrade only once the basics are good to go
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		HandleError(w, r, http.StatusInternalServerError, err, getLoggerFields()...)
		return
	}

	s.mu.Lock()
	s.connections[conn] = struct{}{}
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.connections, conn)
		s.mu.Unlock()
	}()

	// websocket established
	sigConn := NewWSSignalConnection(conn)
	pLogger.Debugw("sending initial response", "response", logger.Proto(initialResponse))
	count, err := sigConn.WriteResponse(initialResponse)
	if err != nil {
		resolveLogger(true)
		pLogger.Warnw("could not write initial response", err)
		return
	}
	signalStats.AddBytes(uint64(count), true)

	pLogger.Debugw(
		"new client WS connected",
		"reconnect", pi.Reconnect,
		"reconnectReason", pi.ReconnectReason,
		"adaptiveStream", pi.AdaptiveStream,
		"selectedNodeID", cr.NodeID,
		"nodeSelectionReason", cr.NodeSelectionReason,
	)

	// handle responses
	go func() {
		defer func() {
			// when the source is terminated, this means Participant.Close had been called and RTC connection is done
			// we would terminate the signal connection as well
			closeMsg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")
			_ = conn.WriteControl(websocket.CloseMessage, closeMsg, time.Now().Add(time.Second))
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
					resolveLogger(true)
					pLogger.Debugw("nothing to read from response source")
					return
				}
				res, ok := msg.(*livekit.SignalResponse)
				if !ok {
					pLogger.Errorw(
						"unexpected message type", nil,
						"type", fmt.Sprintf("%T", msg),
					)
					continue
				}

				switch m := res.Message.(type) {
				case *livekit.SignalResponse_Offer:
					pLogger.Debugw("sending offer", "offer", m)

				case *livekit.SignalResponse_Answer:
					pLogger.Debugw("sending answer", "answer", m)

				case *livekit.SignalResponse_Join:
					pLogger.Debugw("sending join", "join", m)
					signalStats.ResolveRoom(m.Join.GetRoom())
					signalStats.ResolveParticipant(m.Join.GetParticipant())

				case *livekit.SignalResponse_RoomUpdate:
					updateRoomID := livekit.RoomID(m.RoomUpdate.GetRoom().GetSid())
					if updateRoomID != "" {
						roomID = updateRoomID
						resolveLogger(false)
					}
					pLogger.Debugw("sending room update", "roomUpdate", m)
					signalStats.ResolveRoom(m.RoomUpdate.GetRoom())

				case *livekit.SignalResponse_Update:
					pLogger.Debugw("sending participant update", "participantUpdate", m)

				case *livekit.SignalResponse_RoomMoved:
					resetLogger()
					signalStats.Reset()

					roomName = livekit.RoomName(m.RoomMoved.GetRoom().GetName())
					moveRoomID := livekit.RoomID(m.RoomMoved.GetRoom().GetSid())
					if moveRoomID != "" {
						roomID = moveRoomID
					}
					participantIdentity = livekit.ParticipantIdentity(m.RoomMoved.GetParticipant().GetIdentity())
					pID = livekit.ParticipantID(m.RoomMoved.GetParticipant().GetSid())
					resolveLogger(false)

					signalStats.ResolveRoom(m.RoomMoved.GetRoom())
					signalStats.ResolveParticipant(m.RoomMoved.GetParticipant())
					pLogger.Debugw("sending room moved", "roomMoved", m)

				default:
					pLogger.Debugw("sending signal response", "response", m)
				}

				if count, err := sigConn.WriteResponse(res); err != nil {
					pLogger.Warnw("error writing to websocket", err)
					return
				} else {
					signalStats.AddBytes(uint64(count), true)
				}
			}
		}
	}()

	// handle incoming requests from websocket
	for {
		req, count, err := sigConn.ReadRequest()
		if err != nil {
			if IsWebSocketCloseError(err) {
				closedByClient.Store(true)
			} else {
				pLogger.Errorw("error reading from websocket", err)
			}
			return
		}
		signalStats.AddBytes(uint64(count), false)

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
			if perr == nil {
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
			if perr == nil {
				signalStats.AddBytes(uint64(count), true)
			}
		}

		switch m := req.Message.(type) {
		case *livekit.SignalRequest_Offer:
			pLogger.Debugw("received offer", "offer", m)
		case *livekit.SignalRequest_Answer:
			pLogger.Debugw("received answer", "answer", m)
		default:
			pLogger.Debugw("received signal request", "request", m)
		}

		if err := cr.RequestSink.WriteMessage(req); err != nil {
			pLogger.Warnw("error writing to request sink", err)
			return
		}
	}
}

func (s *RTCService) DrainConnections(interval time.Duration) {
	s.mu.Lock()
	conns := maps.Clone(s.connections)
	s.mu.Unlock()

	// jitter drain start
	time.Sleep(time.Duration(rand.Int63n(int64(interval))))

	t := time.NewTicker(interval)
	defer t.Stop()

	for c := range conns {
		_ = c.Close()
		<-t.C
	}
}

type connectionResult struct {
	routing.StartParticipantSignalResults
	Room *livekit.Room
}

func (s *RTCService) startConnection(
	ctx context.Context,
	roomName livekit.RoomName,
	pi routing.ParticipantInit,
	timeout time.Duration,
) (connectionResult, *livekit.SignalResponse, error) {
	var cr connectionResult
	var err error

	if err := s.roomAllocator.SelectRoomNode(ctx, roomName, ""); err != nil {
		return cr, nil, err
	}

	// this needs to be started first *before* using router functions on this node
	cr.StartParticipantSignalResults, err = s.router.StartParticipantSignal(ctx, roomName, pi)
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
