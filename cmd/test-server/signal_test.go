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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
)

const testSecret = "secret"

func newTestServer() *httptest.Server {
	return httptest.NewServer(&mockHandler{regionIndex: 0, apiSecret: testSecret})
}

// mintToken signs a token whose `lk.mock` attribute selects mode (empty → happy).
func mintToken(t *testing.T, mode string) string {
	t.Helper()
	if mode == "" {
		return mintTokenControl(t, nil)
	}
	return mintTokenControl(t, &signalControl{Signal: mode})
}

// mintTokenControl signs a token carrying ctrl as the `lk.mock` attribute (nil → none).
func mintTokenControl(t *testing.T, ctrl *signalControl) string {
	t.Helper()
	at := auth.NewAccessToken("APItest", testSecret).
		SetIdentity("tester").
		SetValidFor(time.Hour).
		SetVideoGrant(&auth.VideoGrant{Room: "test-room", RoomJoin: true})
	if ctrl != nil {
		raw, err := json.Marshal(ctrl)
		if err != nil {
			t.Fatalf("marshal control: %v", err)
		}
		at.SetAttributes(map[string]string{signalControlAttribute: string(raw)})
	}
	tok, err := at.ToJWT()
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	return tok
}

func wsURL(base, path, token string) string {
	u := strings.Replace(base, "http://", "ws://", 1)
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return u + path + sep + "access_token=" + token
}

func dial(t *testing.T, base, path, token string) *websocket.Conn {
	t.Helper()
	c, _, err := websocket.DefaultDialer.Dial(wsURL(base, path, token), nil)
	if err != nil {
		t.Fatalf("dial %s: %v", path, err)
	}
	return c
}

func readResp(t *testing.T, c *websocket.Conn, timeout time.Duration) *livekit.SignalResponse {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(timeout))
	mt, payload, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if mt != websocket.BinaryMessage {
		t.Fatalf("expected binary message, got %d", mt)
	}
	resp := &livekit.SignalResponse{}
	if err := proto.Unmarshal(payload, resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return resp
}

func writeReq(t *testing.T, c *websocket.Conn, req *livekit.SignalRequest) {
	t.Helper()
	payload, err := proto.Marshal(req)
	if err != nil {
		t.Fatalf("marshal req: %v", err)
	}
	if err := c.WriteMessage(websocket.BinaryMessage, payload); err != nil {
		t.Fatalf("write req: %v", err)
	}
}

func TestHappyJoinAndPingPong(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	c := dial(t, srv.URL, "/rtc", mintToken(t, "happy"))
	defer c.Close()

	resp := readResp(t, c, 2*time.Second)
	join := resp.GetJoin()
	if join == nil {
		t.Fatalf("first message not join: %T", resp.Message)
	}
	if join.PingTimeout == 0 || join.PingInterval == 0 {
		t.Fatalf("ping timeout/interval must be non-zero: %d/%d", join.PingTimeout, join.PingInterval)
	}
	if join.ServerInfo == nil || join.Room == nil || join.Participant == nil {
		t.Fatalf("join missing serverInfo/room/participant")
	}
	if join.Room.Name != "test-room" {
		t.Fatalf("room name = %q, want test-room", join.Room.Name)
	}

	// pingReq -> pongResp echoing timestamp
	writeReq(t, c, &livekit.SignalRequest{
		Message: &livekit.SignalRequest_PingReq{PingReq: &livekit.Ping{Timestamp: 12345}},
	})
	pr := readResp(t, c, 2*time.Second)
	if pr.GetPongResp() == nil || pr.GetPongResp().LastPingTimestamp != 12345 {
		t.Fatalf("expected pongResp echoing 12345, got %+v", pr.Message)
	}

	// legacy ping -> pong
	writeReq(t, c, &livekit.SignalRequest{Message: &livekit.SignalRequest_Ping{Ping: 999}})
	pong := readResp(t, c, 2*time.Second)
	if pong.GetPong() == 0 {
		t.Fatalf("expected pong, got %+v", pong.Message)
	}
}

func TestReconnectResponse(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	c := dial(t, srv.URL, "/rtc?reconnect=1", mintToken(t, "happy"))
	defer c.Close()
	resp := readResp(t, c, 2*time.Second)
	if resp.GetReconnect() == nil {
		t.Fatalf("expected reconnect response, got %T", resp.Message)
	}
}

func TestV1PathHappy(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	c := dial(t, srv.URL, "/rtc/v1", mintToken(t, "happy"))
	defer c.Close()
	if readResp(t, c, 2*time.Second).GetJoin() == nil {
		t.Fatal("v1 first message not join")
	}
}

func TestNoPong(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	c := dial(t, srv.URL, "/rtc", mintToken(t, "no_pong"))
	defer c.Close()
	if readResp(t, c, 2*time.Second).GetJoin() == nil {
		t.Fatal("expected join")
	}
	writeReq(t, c, &livekit.SignalRequest{
		Message: &livekit.SignalRequest_PingReq{PingReq: &livekit.Ping{Timestamp: 1}},
	})
	_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if _, _, err := c.ReadMessage(); err == nil {
		t.Fatal("expected no pong (timeout), but got a message")
	}
}

func TestLeaveFirstMessage(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	c := dial(t, srv.URL, "/rtc", mintToken(t, "leave_first_message"))
	defer c.Close()
	if readResp(t, c, 2*time.Second).GetLeave() == nil {
		t.Fatal("expected leave as first message")
	}
}

func TestCloseWhenConnected(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	c := dial(t, srv.URL, "/rtc", mintToken(t, "close_when_connected"))
	defer c.Close()
	if readResp(t, c, 2*time.Second).GetJoin() == nil {
		t.Fatal("expected join")
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err := c.ReadMessage()
	ce, ok := err.(*websocket.CloseError)
	if !ok {
		t.Fatalf("expected close error, got %v", err)
	}
	if ce.Code != websocket.CloseInternalServerErr {
		t.Fatalf("expected close code 1011, got %d", ce.Code)
	}
}

func TestLeaveWhenConnected(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	c := dial(t, srv.URL, "/rtc", mintToken(t, "leave_when_connected"))
	defer c.Close()
	if readResp(t, c, 2*time.Second).GetJoin() == nil {
		t.Fatal("expected join")
	}
	leave := readResp(t, c, 2*time.Second).GetLeave()
	if leave == nil {
		t.Fatal("expected leave after join")
	}
	// Default action is DISCONNECT.
	if leave.Action != livekit.LeaveRequest_DISCONNECT {
		t.Fatalf("default leave action = %v, want DISCONNECT", leave.Action)
	}
}

func TestLeaveActionOverride(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	tok := mintTokenControl(t, &signalControl{
		Signal:      "leave_when_connected",
		LeaveAction: int32(livekit.LeaveRequest_RECONNECT),
	})
	c := dial(t, srv.URL, "/rtc", tok)
	defer c.Close()
	if readResp(t, c, 2*time.Second).GetJoin() == nil {
		t.Fatal("expected join")
	}
	leave := readResp(t, c, 2*time.Second).GetLeave()
	if leave == nil {
		t.Fatal("expected leave after join")
	}
	if leave.Action != livekit.LeaveRequest_RECONNECT {
		t.Fatalf("leave action = %v, want RECONNECT", leave.Action)
	}
}

func TestNoFirstMessage(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	c := dial(t, srv.URL, "/rtc", mintToken(t, "no_first_message"))
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if _, _, err := c.ReadMessage(); err == nil {
		t.Fatal("expected no first message (timeout)")
	}
}

func TestCloseBeforeJoin(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	c := dial(t, srv.URL, "/rtc", mintToken(t, "close_before_join"))
	defer c.Close()
	// First read must be the close, never a join.
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err := c.ReadMessage()
	ce, ok := err.(*websocket.CloseError)
	if !ok {
		t.Fatalf("expected close error before any message, got %v", err)
	}
	if ce.Code != websocket.CloseInternalServerErr {
		t.Fatalf("expected close code 1011, got %d", ce.Code)
	}
	if ce.Text != "" {
		t.Fatalf("expected empty close reason, got %q", ce.Text)
	}
}

func TestDropWhenConnected(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	c := dial(t, srv.URL, "/rtc", mintToken(t, "drop_when_connected"))
	defer c.Close()
	if readResp(t, c, 2*time.Second).GetJoin() == nil {
		t.Fatal("expected join")
	}
	// Abrupt TCP drop → abnormal closure (1006 to a browser): a read error that
	// is not a normal (1000) close.
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err := c.ReadMessage()
	if err == nil {
		t.Fatal("expected read error after abrupt drop")
	}
	if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
		t.Fatalf("expected abnormal (non-1000) closure, got %v", err)
	}
}

func getStatusBody(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestValidateModes(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()

	// happy → 200
	if st, _ := getStatusBody(t, srv.URL+"/rtc/validate?access_token="+mintToken(t, "happy")); st != 200 {
		t.Fatalf("happy validate = %d, want 200", st)
	}
	// v1 validate happy → 200
	if st, _ := getStatusBody(t, srv.URL+"/rtc/v1/validate?access_token="+mintToken(t, "happy")); st != 200 {
		t.Fatalf("v1 happy validate = %d, want 200", st)
	}
	// validate_500 → 500
	if st, _ := getStatusBody(t, srv.URL+"/rtc/validate?access_token="+mintToken(t, "validate_500")); st != 500 {
		t.Fatalf("validate_500 = %d, want 500", st)
	}
	// validate_service_not_found → 404, no marker
	st, body := getStatusBody(t, srv.URL+"/rtc/validate?access_token="+mintToken(t, "validate_service_not_found"))
	if st != 404 || strings.Contains(body, "requested room does not exist") {
		t.Fatalf("service_not_found = %d body=%q", st, body)
	}
	// room_not_found → 404 with marker
	st, body = getStatusBody(t, srv.URL+"/rtc/validate?access_token="+mintToken(t, "room_not_found"))
	if st != 404 || !strings.Contains(body, "requested room does not exist") {
		t.Fatalf("room_not_found = %d body=%q", st, body)
	}
	// bad token → 401
	if st, _ := getStatusBody(t, srv.URL+"/rtc/validate?access_token=not-a-jwt"); st != 401 {
		t.Fatalf("bad token validate = %d, want 401", st)
	}
	// missing token → 401
	if st, _ := getStatusBody(t, srv.URL+"/rtc/validate"); st != 401 {
		t.Fatalf("missing token validate = %d, want 401", st)
	}
}

func TestValidateErrorModesRefuseWS(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	// Validate-error modes must refuse the upgrade so the client falls back to validate.
	_, resp, err := websocket.DefaultDialer.Dial(wsURL(srv.URL, "/rtc", mintToken(t, "validate_500")), nil)
	if err == nil {
		t.Fatal("expected WS dial to fail for validate_500")
	}
	if resp == nil || resp.StatusCode != 500 {
		t.Fatalf("expected 500 on WS refuse, got %v", resp)
	}
}

func TestValidateCORSHeader(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	// ACAO must be present on every status so a browser fetch can read it.
	cases := map[string]string{
		"happy":          mintToken(t, "happy"),          // 200
		"bad":            "bad",                          // 401
		"room_not_found": mintToken(t, "room_not_found"), // 404
		"validate_500":   mintToken(t, "validate_500"),   // 500
	}
	for name, tok := range cases {
		resp, err := http.Get(srv.URL + "/rtc/validate?access_token=" + tok)
		if err != nil {
			t.Fatalf("%s: get: %v", name, err)
		}
		got := resp.Header.Get("Access-Control-Allow-Origin")
		resp.Body.Close()
		if got != "*" {
			t.Fatalf("%s (status %d): ACAO = %q, want *", name, resp.StatusCode, got)
		}
	}
}

func TestWSCrossOrigin(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	// A mismatched browser Origin must still upgrade (CheckOrigin allows any).
	hdr := http.Header{}
	hdr.Set("Origin", "http://localhost:5173")
	c, _, err := websocket.DefaultDialer.Dial(wsURL(srv.URL, "/rtc", mintToken(t, "happy")), hdr)
	if err != nil {
		t.Fatalf("cross-origin dial: %v", err)
	}
	defer c.Close()
	if readResp(t, c, 2*time.Second).GetJoin() == nil {
		t.Fatal("expected join on cross-origin WS")
	}
}

func TestLeaveDuringReconnect(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	c := dial(t, srv.URL, "/rtc?reconnect=1", mintToken(t, "leave_during_reconnect"))
	defer c.Close()
	if readResp(t, c, 2*time.Second).GetLeave() == nil {
		t.Fatal("expected leave on reconnect")
	}
}
