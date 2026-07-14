// Copyright 2026 LiveKit, Inc.
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

package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
)

// Signal endpoints (/rtc, /rtc/v1 and their /validate counterparts) let SDKs
// exercise end-to-end WebSocket signal behavior. Per-connection behavior is
// selected by the `lk.mock` participant attribute (see signalControl), so tests
// need no shared state. The client fetches validate only when the WS fails to
// open, so validate-error modes refuse the upgrade with the matching status and
// let that fetch return the definitive status/body.

const (
	// Short keepalive (seconds, sent in the JoinResponse) so timeout tests run fast.
	signalPingInterval = 1
	signalPingTimeout  = 3

	// Delay after join before close_when_connected / leave_when_connected act,
	// giving the client time to mark the connection established.
	connectedDelay = 200 * time.Millisecond
)

// Behavior modes, selected by the `lk.mock` attribute's `signal` field. Any
// unknown/absent signal behaves as the happy path.
const (
	// Validate-endpoint modes (the WS upgrade is refused with the same status
	// so the client falls back to the validate fetch).
	modeValidate500     = "validate_500"               // validate → 500
	modeServiceNotFound = "validate_service_not_found" // validate → 404, generic body
	modeRoomNotFound    = "room_not_found"             // validate → 404, "requested room does not exist"

	// WebSocket signal modes (validate → 200; behavior is on the WS).
	modeHappy                = "happy"                  // join, pong, clean close on client leave
	modeNoFirstMessage       = "no_first_message"       // accept WS, send nothing
	modeNoPong               = "no_pong"                // send join, never pong
	modeCloseBeforeJoin      = "close_before_join"      // clean close 1011 before any first message
	modeCloseWhenConnected   = "close_when_connected"   // send join, then clean close 1011
	modeDropWhenConnected    = "drop_when_connected"    // send join, then abrupt TCP drop (1006)
	modeLeaveWhenConnected   = "leave_when_connected"   // send join, then LeaveRequest
	modeLeaveFirstMessage    = "leave_first_message"    // LeaveRequest as first message
	modeLeaveDuringReconnect = "leave_during_reconnect" // on reconnect=1, LeaveRequest first
	modeDropOnClose          = "drop_on_close"          // happy, but drop TCP on the client's close frame instead of completing the handshake
)

const signalControlAttribute = "lk.mock"

// signalControl is the JSON value of the `lk.mock` attribute:
// {"signal":"<mode>","leaveAction":<int|name>}. leaveAction is optional
// (a LeaveRequest_Action, given as the number or the enum name e.g.
// "RECONNECT"; absent/0 = DISCONNECT) and sets the action on emitted leaves.
type signalControl struct {
	Signal      string           `json:"signal"`
	LeaveAction leaveActionValue `json:"leaveAction"`
}

// leaveActionValue is a LeaveRequest_Action that unmarshals from either a JSON
// number (2) or an enum name ("RECONNECT", case-insensitive). Anything
// unrecognized decodes to 0 (DISCONNECT) rather than failing the whole control.
type leaveActionValue livekit.LeaveRequest_Action

func (v *leaveActionValue) UnmarshalJSON(b []byte) error {
	var n int32
	if json.Unmarshal(b, &n) == nil {
		*v = leaveActionValue(n)
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	*v = leaveActionValue(livekit.LeaveRequest_Action_value[strings.ToUpper(s)])
	return nil
}

// parseSignalControl parses the `lk.mock` attribute value; absent/invalid → zero.
func parseSignalControl(grants *auth.ClaimGrants) signalControl {
	if grants == nil {
		return signalControl{}
	}
	raw := grants.Attributes[signalControlAttribute]
	if raw == "" {
		return signalControl{}
	}
	var ctrl signalControl
	if err := json.Unmarshal([]byte(raw), &ctrl); err != nil {
		return signalControl{}
	}
	return ctrl
}

// signalMode returns the mode from the `lk.mock` `signal` field; unknown/absent → happy.
func signalMode(grants *auth.ClaimGrants) string {
	switch ctrl := parseSignalControl(grants); ctrl.Signal {
	case modeValidate500, modeServiceNotFound, modeRoomNotFound,
		modeHappy, modeNoFirstMessage, modeNoPong,
		modeCloseBeforeJoin, modeCloseWhenConnected, modeDropWhenConnected,
		modeLeaveWhenConnected, modeLeaveFirstMessage, modeLeaveDuringReconnect,
		modeDropOnClose:
		return ctrl.Signal
	default:
		return modeHappy
	}
}

func isSignalPath(path string) bool {
	return path == "/rtc" || path == "/rtc/v1"
}

func isValidatePath(path string) bool {
	return path == "/rtc/validate" || path == "/rtc/v1/validate"
}

var signalUpgrader = websocket.Upgrader{
	EnableCompression: true,
	// Auth is via the access token, so allow any origin.
	CheckOrigin: func(r *http.Request) bool { return true },
}

// verifySignalToken reads the access token (access_token query param or Bearer
// header) and verifies it against the mock's API secret.
func (h *mockHandler) verifySignalToken(r *http.Request) (*auth.ClaimGrants, error) {
	token := r.FormValue("access_token")
	if token == "" {
		token = strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	}
	v, err := auth.ParseAPIToken(token)
	if err != nil {
		return nil, err
	}
	_, grants, err := v.Verify(h.apiSecret)
	if err != nil {
		return nil, err
	}
	return grants, nil
}

func grantRoom(grants *auth.ClaimGrants) string {
	if grants == nil || grants.Video == nil {
		return ""
	}
	return grants.Video.Room
}

// handleValidate verifies the JWT (bad/expired/missing → 401), then returns the
// status the mode dictates.
func (h *mockHandler) handleValidate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")

	grants, err := h.verifySignalToken(r)
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("invalid token: " + err.Error()))
		return
	}

	switch signalMode(grants) {
	case modeValidate500:
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal server error"))
	case modeServiceNotFound:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("404 page not found"))
	case modeRoomNotFound:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("requested room does not exist"))
	default:
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("success"))
	}
}

// handleSignal verifies the token, applies validate-error modes by refusing the
// upgrade, else upgrades and runs the selected behavior. The v1 publisher offer
// is ignored.
func (h *mockHandler) handleSignal(w http.ResponseWriter, r *http.Request) {
	grants, err := h.verifySignalToken(r)
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("invalid token"))
		return
	}

	mode := signalMode(grants)

	switch mode {
	case modeValidate500:
		w.WriteHeader(http.StatusInternalServerError)
		return
	case modeServiceNotFound, modeRoomNotFound:
		w.WriteHeader(http.StatusNotFound)
		return
	}

	reconnect := r.URL.Query().Get("reconnect") == "1"

	conn, err := signalUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()

	h.runSignal(conn, mode, reconnect, grants)
}

// runSignal drives one WebSocket connection according to mode.
func (h *mockHandler) runSignal(conn *websocket.Conn, mode string, reconnect bool, grants *auth.ClaimGrants) {
	writeResp := func(msg *livekit.SignalResponse) error {
		payload, err := proto.Marshal(msg)
		if err != nil {
			return err
		}
		return conn.WriteMessage(websocket.BinaryMessage, payload)
	}

	leaveAction := livekit.LeaveRequest_Action(parseSignalControl(grants).LeaveAction)

	// drainUntilClosed reads/discards until the peer closes, keeping the socket open.
	drainUntilClosed := func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}

	// Modes that decide the very first message.
	switch mode {
	case modeNoFirstMessage:
		drainUntilClosed()
		return
	case modeCloseBeforeJoin:
		time.Sleep(50 * time.Millisecond)
		msg := websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "")
		_ = conn.WriteControl(websocket.CloseMessage, msg, time.Now().Add(time.Second))
		return
	case modeLeaveFirstMessage:
		_ = writeResp(leaveResponse(leaveAction))
		drainUntilClosed()
		return
	case modeLeaveDuringReconnect:
		if reconnect {
			_ = writeResp(leaveResponse(leaveAction))
			drainUntilClosed()
			return
		}
		// Non-reconnect connections fall through to the happy path.
	}

	// First message: reconnect response on a resume, join otherwise.
	if reconnect {
		if err := writeResp(reconnectResponse(h.regionIndex)); err != nil {
			return
		}
	} else {
		if err := writeResp(joinResponse(h.regionIndex, grants)); err != nil {
			return
		}
	}

	// Post-join behaviors.
	switch mode {
	case modeCloseWhenConnected:
		time.Sleep(connectedDelay)
		msg := websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "mock close_when_connected")
		_ = conn.WriteControl(websocket.CloseMessage, msg, time.Now().Add(time.Second))
		return
	case modeDropWhenConnected:
		time.Sleep(connectedDelay)
		_ = conn.UnderlyingConn().Close()
		return
	case modeLeaveWhenConnected:
		time.Sleep(connectedDelay)
		_ = writeResp(leaveResponse(leaveAction))
	}

	// drop_on_close: when the client starts a clean close handshake, drop the
	// TCP connection instead of replying with a close frame, so the client
	// observes an abnormal closure while it is disconnecting.
	if mode == modeDropOnClose {
		conn.SetCloseHandler(func(code int, text string) error {
			return conn.UnderlyingConn().Close()
		})
	}

	// Read loop: pong to pings (unless no_pong), clean close on client leave.
	for {
		mt, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if mt != websocket.BinaryMessage {
			continue
		}
		req := &livekit.SignalRequest{}
		if err := proto.Unmarshal(payload, req); err != nil {
			continue
		}
		switch m := req.Message.(type) {
		case *livekit.SignalRequest_Ping:
			if mode != modeNoPong {
				_ = writeResp(&livekit.SignalResponse{
					Message: &livekit.SignalResponse_Pong{Pong: time.Now().UnixMilli()},
				})
			}
		case *livekit.SignalRequest_PingReq:
			if mode != modeNoPong {
				_ = writeResp(&livekit.SignalResponse{
					Message: &livekit.SignalResponse_PongResp{
						PongResp: &livekit.Pong{
							LastPingTimestamp: m.PingReq.Timestamp,
							Timestamp:         time.Now().UnixMilli(),
						},
					},
				})
			}
		case *livekit.SignalRequest_UpdateMetadata:
			// Ack like the real server does, so SDK tests can observe that a
			// (possibly queued) request actually reached the server.
			_ = writeResp(&livekit.SignalResponse{
				Message: &livekit.SignalResponse_RequestResponse{
					RequestResponse: &livekit.RequestResponse{
						RequestId: m.UpdateMetadata.RequestId,
						Reason:    livekit.RequestResponse_OK,
					},
				},
			})
		case *livekit.SignalRequest_Leave:
			msg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")
			_ = conn.WriteControl(websocket.CloseMessage, msg, time.Now().Add(time.Second))
			return
		}
	}
}

func serverInfo(regionIndex int) *livekit.ServerInfo {
	return &livekit.ServerInfo{
		Edition:  livekit.ServerInfo_Standard,
		Version:  "mock",
		Protocol: 15,
		Region:   regionName(regionIndex),
		NodeId:   "MOCK_NODE",
	}
}

func regionName(regionIndex int) string {
	return "region-" + strconv.Itoa(regionIndex)
}

// joinResponse builds the initial JoinResponse (non-zero ping config so the
// client arms keepalive).
func joinResponse(regionIndex int, grants *auth.ClaimGrants) *livekit.SignalResponse {
	room := grantRoom(grants)
	identity := "mock-participant"
	name := ""
	if grants != nil {
		if grants.Identity != "" {
			identity = grants.Identity
		}
		name = grants.Name
	}
	return &livekit.SignalResponse{
		Message: &livekit.SignalResponse_Join{
			Join: &livekit.JoinResponse{
				Room: &livekit.Room{
					Sid:  "RM_MOCK",
					Name: room,
				},
				Participant: &livekit.ParticipantInfo{
					Sid:      "PA_MOCK",
					Identity: identity,
					Name:     name,
					State:    livekit.ParticipantInfo_JOINED,
				},
				PingInterval:  signalPingInterval,
				PingTimeout:   signalPingTimeout,
				ServerInfo:    serverInfo(regionIndex),
				ServerVersion: "mock",
				ServerRegion:  regionName(regionIndex),
			},
		},
	}
}

func reconnectResponse(regionIndex int) *livekit.SignalResponse {
	return &livekit.SignalResponse{
		Message: &livekit.SignalResponse_Reconnect{
			Reconnect: &livekit.ReconnectResponse{
				ServerInfo: serverInfo(regionIndex),
			},
		},
	}
}

func leaveResponse(action livekit.LeaveRequest_Action) *livekit.SignalResponse {
	return &livekit.SignalResponse{
		Message: &livekit.SignalResponse_Leave{
			Leave: &livekit.LeaveRequest{
				Reason: livekit.DisconnectReason_SERVER_SHUTDOWN,
				Action: action,
			},
		},
	}
}
