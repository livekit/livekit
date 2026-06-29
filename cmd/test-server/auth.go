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
	"net/http"
	"strings"

	"github.com/livekit/protocol/auth"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// The mock enforces the same token permissions the real LiveKit server requires
// for each API method (see pkg/service/auth.go in livekit/livekit). The point is
// to verify that SDKs attach the correct grants automatically. Tokens are parsed
// and verified with the protocol's own auth helpers (the same code the server
// uses), against the mock's configured API secret (default "secret", matching
// `livekit-server --dev`); set --api-secret / LK_TEST_SERVER_API_SECRET to change it.
//
// Set X-Lk-Mock-Skip-Auth: true to bypass enforcement for tests that aren't
// about permissions (e.g. region-failover tests using a placeholder token).

// perm describes the grants a method requires. roomAdmin additionally requires
// the token's room to match the request's room; destRoom further requires the
// token's destinationRoom to match the request's destination_room.
type perm struct {
	roomCreate   bool
	roomList     bool
	roomRecord   bool
	ingressAdmin bool
	roomAdmin    bool
	destRoom     bool
	sipAdmin     bool
	sipCall      bool
}

// methodPerms maps "<package>.<Service>/<Method>" to its required grants,
// matching the Ensure*Permission checks in the real server's services.
var methodPerms = map[string]perm{
	// RoomService
	"livekit.RoomService/CreateRoom":          {roomCreate: true},
	"livekit.RoomService/DeleteRoom":          {roomCreate: true},
	"livekit.RoomService/ListRooms":           {roomList: true},
	"livekit.RoomService/ListParticipants":    {roomAdmin: true},
	"livekit.RoomService/GetParticipant":      {roomAdmin: true},
	"livekit.RoomService/RemoveParticipant":   {roomAdmin: true},
	"livekit.RoomService/MutePublishedTrack":  {roomAdmin: true},
	"livekit.RoomService/UpdateParticipant":   {roomAdmin: true},
	"livekit.RoomService/UpdateSubscriptions": {roomAdmin: true},
	"livekit.RoomService/SendData":            {roomAdmin: true},
	"livekit.RoomService/UpdateRoomMetadata":  {roomAdmin: true},
	"livekit.RoomService/ForwardParticipant":  {destRoom: true},
	"livekit.RoomService/MoveParticipant":     {destRoom: true},
	"livekit.RoomService/PerformRpc":          {roomAdmin: true},

	// Egress — all require record permission
	"livekit.Egress/StartEgress":               {roomRecord: true},
	"livekit.Egress/StartRoomCompositeEgress":  {roomRecord: true},
	"livekit.Egress/StartWebEgress":            {roomRecord: true},
	"livekit.Egress/StartParticipantEgress":    {roomRecord: true},
	"livekit.Egress/StartTrackCompositeEgress": {roomRecord: true},
	"livekit.Egress/StartTrackEgress":          {roomRecord: true},
	"livekit.Egress/UpdateLayout":              {roomRecord: true},
	"livekit.Egress/UpdateStream":              {roomRecord: true},
	"livekit.Egress/ListEgress":                {roomRecord: true},
	"livekit.Egress/StopEgress":                {roomRecord: true},

	// Ingress — all require ingress admin
	"livekit.Ingress/CreateIngress": {ingressAdmin: true},
	"livekit.Ingress/UpdateIngress": {ingressAdmin: true},
	"livekit.Ingress/ListIngress":   {ingressAdmin: true},
	"livekit.Ingress/DeleteIngress": {ingressAdmin: true},

	// SIP — trunk/dispatch administration requires sip.admin
	"livekit.SIP/CreateSIPInboundTrunk":  {sipAdmin: true},
	"livekit.SIP/CreateSIPOutboundTrunk": {sipAdmin: true},
	"livekit.SIP/UpdateSIPInboundTrunk":  {sipAdmin: true},
	"livekit.SIP/UpdateSIPOutboundTrunk": {sipAdmin: true},
	"livekit.SIP/GetSIPInboundTrunk":     {sipAdmin: true},
	"livekit.SIP/GetSIPOutboundTrunk":    {sipAdmin: true},
	"livekit.SIP/ListSIPTrunk":           {sipAdmin: true},
	"livekit.SIP/ListSIPInboundTrunk":    {sipAdmin: true},
	"livekit.SIP/ListSIPOutboundTrunk":   {sipAdmin: true},
	"livekit.SIP/DeleteSIPTrunk":         {sipAdmin: true},
	"livekit.SIP/CreateSIPDispatchRule":  {sipAdmin: true},
	"livekit.SIP/UpdateSIPDispatchRule":  {sipAdmin: true},
	"livekit.SIP/ListSIPDispatchRule":    {sipAdmin: true},
	"livekit.SIP/DeleteSIPDispatchRule":  {sipAdmin: true},
	// Placing a call requires sip.call; transfer also requires room admin.
	"livekit.SIP/CreateSIPParticipant":   {sipCall: true},
	"livekit.SIP/TransferSIPParticipant": {sipCall: true, roomAdmin: true},

	// AgentDispatch — room admin scoped to the dispatch's room
	"livekit.AgentDispatchService/CreateDispatch": {roomAdmin: true},
	"livekit.AgentDispatchService/DeleteDispatch": {roomAdmin: true},
	"livekit.AgentDispatchService/ListDispatch":   {roomAdmin: true},

	// Connector (cloud) — initiating a call requires room create
	"livekit.Connector/DialWhatsAppCall":       {roomCreate: true},
	"livekit.Connector/DisconnectWhatsAppCall": {roomCreate: true},
	"livekit.Connector/ConnectWhatsAppCall":    {roomCreate: true},
	"livekit.Connector/AcceptWhatsAppCall":     {roomCreate: true},
	"livekit.Connector/ConnectTwilioCall":      {roomCreate: true},
}

// authorize enforces the permissions a method requires. It returns the HTTP
// status and Twirp error code to send (0, "" means authorized / not enforced).
func (h *mockHandler) authorize(key string, r *http.Request, req proto.Message) (int, string) {
	if strings.EqualFold(r.Header.Get(headerSkipAuth), "true") {
		return 0, ""
	}
	p, known := methodPerms[key]
	if !known {
		return 0, "" // unknown/future method: don't enforce
	}

	grants, err := h.verifyToken(r.Header.Get("Authorization"))
	if err != nil {
		// Missing, malformed, or improperly-signed token, like the real server.
		return http.StatusUnauthorized, "unauthenticated"
	}
	if !p.satisfiedBy(grants, req) {
		return http.StatusForbidden, "permission_denied"
	}
	return 0, ""
}

// verifyToken parses and verifies a "Bearer <jwt>" header using the protocol's
// own auth helpers (the same path the real server uses), returning the grants.
func (h *mockHandler) verifyToken(authorization string) (*auth.ClaimGrants, error) {
	token := strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer "))
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

func (p perm) satisfiedBy(g *auth.ClaimGrants, req proto.Message) bool {
	v, s := g.Video, g.SIP
	if p.roomCreate && (v == nil || !v.RoomCreate) {
		return false
	}
	if p.roomList && (v == nil || !v.RoomList) {
		return false
	}
	if p.roomRecord && (v == nil || !v.RoomRecord) {
		return false
	}
	if p.ingressAdmin && (v == nil || !v.IngressAdmin) {
		return false
	}
	if p.sipAdmin && (s == nil || !s.Admin) {
		return false
	}
	if p.sipCall && (s == nil || !s.Call) {
		return false
	}
	if p.roomAdmin || p.destRoom {
		if v == nil || !v.RoomAdmin {
			return false
		}
		if room := requestRoom(req); room != "" && v.Room != room {
			return false
		}
	}
	if p.destRoom {
		if dest := requestString(req, "destination_room"); dest != "" && v.DestinationRoom != dest {
			return false
		}
	}
	return true
}

// requestRoom reads the room name a request targets, trying the common "room"
// and "room_name" fields.
func requestRoom(req proto.Message) string {
	if v := requestString(req, "room"); v != "" {
		return v
	}
	return requestString(req, "room_name")
}

func requestString(req proto.Message, field string) string {
	if req == nil {
		return ""
	}
	m := req.ProtoReflect()
	fd := m.Descriptor().Fields().ByName(protoreflect.Name(field))
	if fd == nil || fd.Kind() != protoreflect.StringKind || fd.IsList() {
		return ""
	}
	return m.Get(fd).String()
}
