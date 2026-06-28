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
	"io"
	"net/http"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/utils/protojson"
)

// apiSpec captures the request and response message types for one Twirp method,
// so the mock can decode the incoming request and build a typed response.
type apiSpec struct {
	newReq  func() proto.Message
	newResp func() proto.Message
}

// ptrMsg constrains a pointer type that is also a proto.Message, letting reg
// construct fresh request/response values generically.
type ptrMsg[T any] interface {
	*T
	proto.Message
}

// apiHandlers maps "<package>.<Service>/<Method>" to its message types. It
// covers the full LiveKit API surface; see init below.
var apiHandlers = map[string]apiSpec{}

func reg[ReqT, RespT any, Req ptrMsg[ReqT], Resp ptrMsg[RespT]](key string) {
	apiHandlers[key] = apiSpec{
		newReq:  func() proto.Message { return Req(new(ReqT)) },
		newResp: func() proto.Message { return Resp(new(RespT)) },
	}
}

func init() {
	// RoomService
	reg[livekit.CreateRoomRequest, livekit.Room]("livekit.RoomService/CreateRoom")
	reg[livekit.ListRoomsRequest, livekit.ListRoomsResponse]("livekit.RoomService/ListRooms")
	reg[livekit.DeleteRoomRequest, livekit.DeleteRoomResponse]("livekit.RoomService/DeleteRoom")
	reg[livekit.ListParticipantsRequest, livekit.ListParticipantsResponse]("livekit.RoomService/ListParticipants")
	reg[livekit.RoomParticipantIdentity, livekit.ParticipantInfo]("livekit.RoomService/GetParticipant")
	reg[livekit.RoomParticipantIdentity, livekit.RemoveParticipantResponse]("livekit.RoomService/RemoveParticipant")
	reg[livekit.MuteRoomTrackRequest, livekit.MuteRoomTrackResponse]("livekit.RoomService/MutePublishedTrack")
	reg[livekit.UpdateParticipantRequest, livekit.ParticipantInfo]("livekit.RoomService/UpdateParticipant")
	reg[livekit.UpdateSubscriptionsRequest, livekit.UpdateSubscriptionsResponse]("livekit.RoomService/UpdateSubscriptions")
	reg[livekit.SendDataRequest, livekit.SendDataResponse]("livekit.RoomService/SendData")
	reg[livekit.UpdateRoomMetadataRequest, livekit.Room]("livekit.RoomService/UpdateRoomMetadata")
	reg[livekit.ForwardParticipantRequest, livekit.ForwardParticipantResponse]("livekit.RoomService/ForwardParticipant")
	reg[livekit.MoveParticipantRequest, livekit.MoveParticipantResponse]("livekit.RoomService/MoveParticipant")
	reg[livekit.PerformRpcRequest, livekit.PerformRpcResponse]("livekit.RoomService/PerformRpc")

	// Egress
	reg[livekit.StartEgressRequest, livekit.EgressInfo]("livekit.Egress/StartEgress")
	reg[livekit.UpdateLayoutRequest, livekit.EgressInfo]("livekit.Egress/UpdateLayout")
	reg[livekit.UpdateStreamRequest, livekit.EgressInfo]("livekit.Egress/UpdateStream")
	reg[livekit.ListEgressRequest, livekit.ListEgressResponse]("livekit.Egress/ListEgress")
	reg[livekit.StopEgressRequest, livekit.EgressInfo]("livekit.Egress/StopEgress")
	reg[livekit.RoomCompositeEgressRequest, livekit.EgressInfo]("livekit.Egress/StartRoomCompositeEgress")
	reg[livekit.WebEgressRequest, livekit.EgressInfo]("livekit.Egress/StartWebEgress")
	reg[livekit.ParticipantEgressRequest, livekit.EgressInfo]("livekit.Egress/StartParticipantEgress")
	reg[livekit.TrackCompositeEgressRequest, livekit.EgressInfo]("livekit.Egress/StartTrackCompositeEgress")
	reg[livekit.TrackEgressRequest, livekit.EgressInfo]("livekit.Egress/StartTrackEgress")

	// Ingress
	reg[livekit.CreateIngressRequest, livekit.IngressInfo]("livekit.Ingress/CreateIngress")
	reg[livekit.UpdateIngressRequest, livekit.IngressInfo]("livekit.Ingress/UpdateIngress")
	reg[livekit.ListIngressRequest, livekit.ListIngressResponse]("livekit.Ingress/ListIngress")
	reg[livekit.DeleteIngressRequest, livekit.IngressInfo]("livekit.Ingress/DeleteIngress")

	// SIP
	reg[livekit.ListSIPTrunkRequest, livekit.ListSIPTrunkResponse]("livekit.SIP/ListSIPTrunk")
	reg[livekit.CreateSIPInboundTrunkRequest, livekit.SIPInboundTrunkInfo]("livekit.SIP/CreateSIPInboundTrunk")
	reg[livekit.CreateSIPOutboundTrunkRequest, livekit.SIPOutboundTrunkInfo]("livekit.SIP/CreateSIPOutboundTrunk")
	reg[livekit.UpdateSIPInboundTrunkRequest, livekit.SIPInboundTrunkInfo]("livekit.SIP/UpdateSIPInboundTrunk")
	reg[livekit.UpdateSIPOutboundTrunkRequest, livekit.SIPOutboundTrunkInfo]("livekit.SIP/UpdateSIPOutboundTrunk")
	reg[livekit.GetSIPInboundTrunkRequest, livekit.GetSIPInboundTrunkResponse]("livekit.SIP/GetSIPInboundTrunk")
	reg[livekit.GetSIPOutboundTrunkRequest, livekit.GetSIPOutboundTrunkResponse]("livekit.SIP/GetSIPOutboundTrunk")
	reg[livekit.ListSIPInboundTrunkRequest, livekit.ListSIPInboundTrunkResponse]("livekit.SIP/ListSIPInboundTrunk")
	reg[livekit.ListSIPOutboundTrunkRequest, livekit.ListSIPOutboundTrunkResponse]("livekit.SIP/ListSIPOutboundTrunk")
	reg[livekit.DeleteSIPTrunkRequest, livekit.SIPTrunkInfo]("livekit.SIP/DeleteSIPTrunk")
	reg[livekit.CreateSIPDispatchRuleRequest, livekit.SIPDispatchRuleInfo]("livekit.SIP/CreateSIPDispatchRule")
	reg[livekit.UpdateSIPDispatchRuleRequest, livekit.SIPDispatchRuleInfo]("livekit.SIP/UpdateSIPDispatchRule")
	reg[livekit.ListSIPDispatchRuleRequest, livekit.ListSIPDispatchRuleResponse]("livekit.SIP/ListSIPDispatchRule")
	reg[livekit.DeleteSIPDispatchRuleRequest, livekit.SIPDispatchRuleInfo]("livekit.SIP/DeleteSIPDispatchRule")
	reg[livekit.CreateSIPParticipantRequest, livekit.SIPParticipantInfo]("livekit.SIP/CreateSIPParticipant")
	reg[livekit.TransferSIPParticipantRequest, emptypb.Empty]("livekit.SIP/TransferSIPParticipant")

	// Connector
	reg[livekit.DialWhatsAppCallRequest, livekit.DialWhatsAppCallResponse]("livekit.Connector/DialWhatsAppCall")
	reg[livekit.DisconnectWhatsAppCallRequest, livekit.DisconnectWhatsAppCallResponse]("livekit.Connector/DisconnectWhatsAppCall")
	reg[livekit.ConnectWhatsAppCallRequest, livekit.ConnectWhatsAppCallResponse]("livekit.Connector/ConnectWhatsAppCall")
	reg[livekit.AcceptWhatsAppCallRequest, livekit.AcceptWhatsAppCallResponse]("livekit.Connector/AcceptWhatsAppCall")
	reg[livekit.ConnectTwilioCallRequest, livekit.ConnectTwilioCallResponse]("livekit.Connector/ConnectTwilioCall")
}

// serveAPI handles a Twirp call end to end: decode the request, enforce the
// method's required permissions (like the real server's auth middleware), apply
// any region-failure injection, then serve a populated response.
func (h *mockHandler) serveAPI(w http.ResponseWriter, r *http.Request) {
	json := strings.Contains(r.Header.Get("Content-Type"), "json")
	key := strings.TrimPrefix(r.URL.Path, h.twirpPrefix+"/")
	spec, known := apiHandlers[key]

	// Decode the request up front — needed both to enforce room-scoped grants
	// and to build the echoed response.
	var req proto.Message
	if known {
		body, _ := io.ReadAll(r.Body)
		req = spec.newReq()
		if json {
			_ = protojson.Unmarshal(body, req)
		} else {
			_ = proto.Unmarshal(body, req)
		}
	}

	// Permission enforcement comes first, mirroring the real server.
	if status, code := authorize(key, r, req); status != 0 {
		writeTwirpErrorCode(w, status, code, "mock: "+code)
		return
	}

	if h.shouldFail(r) {
		h.fail(w, r)
		return
	}

	h.writeAPIResponse(w, r, json, known, req, spec)
}

// writeAPIResponse serves a populated, type-correct response for a known API
// method. The response is the reflection-populated default unless the request
// carries an X-Lk-Mock-Response header (protojson), which overrides it
// entirely. Content type (protobuf vs JSON) mirrors the request.
func (h *mockHandler) writeAPIResponse(w http.ResponseWriter, r *http.Request, json, known bool, req proto.Message, spec apiSpec) {
	w.Header().Set(headerRegion, strconv.Itoa(h.regionIndex))

	if !known {
		// Unknown/future method: an empty body still decodes to a valid default
		// message in every Twirp client.
		writeEmptySuccess(w, json)
		return
	}

	resp := spec.newResp()
	if override := r.Header.Get(headerResponse); override != "" {
		if err := protojson.Unmarshal([]byte(override), resp); err != nil {
			// Malformed override: fall back to the populated default.
			resp = spec.newResp()
			populateMessage(resp.ProtoReflect(), req.ProtoReflect(), 1)
		}
	} else {
		populateMessage(resp.ProtoReflect(), req.ProtoReflect(), 1)
	}

	if json {
		out, _ := protojson.Marshal(resp)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
	} else {
		out, _ := proto.Marshal(resp)
		w.Header().Set("Content-Type", "application/protobuf")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
	}
}

func writeEmptySuccess(w http.ResponseWriter, json bool) {
	if json {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	} else {
		w.Header().Set("Content-Type", "application/protobuf")
		w.WriteHeader(http.StatusOK)
	}
}

// populateMessage fills a response message with plausible values: it echoes
// scalar fields that share a name with the request, assigns placeholder values
// to id/sid fields, and adds one element to repeated-message (list) fields so
// list endpoints return non-empty results. depth bounds list-element nesting.
func populateMessage(m protoreflect.Message, req protoreflect.Message, depth int) {
	fields := m.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)

		// Echo a same-named scalar field from the request (e.g. name, metadata,
		// identity, room, timeouts).
		if req != nil && fd.Cardinality() != protoreflect.Repeated && isScalarKind(fd.Kind()) {
			if rf := req.Descriptor().Fields().ByName(fd.Name()); rf != nil &&
				rf.Kind() == fd.Kind() && rf.Cardinality() != protoreflect.Repeated && req.Has(rf) {
				m.Set(fd, req.Get(rf))
				continue
			}
		}

		// Give id/sid-like string fields a deterministic placeholder.
		if fd.Kind() == protoreflect.StringKind && fd.Cardinality() != protoreflect.Repeated && !m.Has(fd) {
			n := string(fd.Name())
			if n == "id" || n == "sid" || strings.HasSuffix(n, "_id") || strings.HasSuffix(n, "_sid") {
				m.Set(fd, protoreflect.ValueOfString("MOCK_"+strings.ToUpper(n)))
				continue
			}
		}

		// Populate list endpoints with a single element so clients see results.
		if depth > 0 && fd.IsList() && fd.Kind() == protoreflect.MessageKind {
			list := m.Mutable(fd).List()
			elem := list.NewElement()
			populateMessage(elem.Message(), nil, depth-1)
			list.Append(elem)
		}
	}
}

func isScalarKind(k protoreflect.Kind) bool {
	switch k {
	case protoreflect.BoolKind,
		protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Uint32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Uint64Kind,
		protoreflect.Sfixed32Kind, protoreflect.Fixed32Kind,
		protoreflect.Sfixed64Kind, protoreflect.Fixed64Kind,
		protoreflect.FloatKind, protoreflect.DoubleKind,
		protoreflect.StringKind, protoreflect.BytesKind:
		return true
	default:
		return false
	}
}
