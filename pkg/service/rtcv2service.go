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
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/signalling"
	"github.com/livekit/psrpc"
	"google.golang.org/protobuf/proto"
)

var (
	errFragmentsInHTTP    = errors.New("should not get fragments via HTTP request")
	errUnknownMessageType = errors.New("unknown message type")
)

const (
	cRTCv2Path              = "/rtc/v2"
	cRTCv2ValidatePath      = "/rtc/v2/validate"
	cRTCv2ParticipantIDPath = "/rtc/v2/{participant_id}"
)

type RTCv2Service struct {
	http.Handler

	limits        config.LimitConfig
	roomAllocator RoomAllocator
	router        routing.MessageRouter

	topicFormatter            rpc.TopicFormatter
	signalv2ParticipantClient rpc.TypedSignalv2ParticipantClient
}

func NewRTCv2Service(
	config *config.Config,
	roomAllocator RoomAllocator,
	router routing.MessageRouter,
	topicFormatter rpc.TopicFormatter,
	signalv2ParticipantClient rpc.TypedSignalv2ParticipantClient,
) *RTCv2Service {
	return &RTCv2Service{
		limits:                    config.Limit,
		router:                    router,
		roomAllocator:             roomAllocator,
		topicFormatter:            topicFormatter,
		signalv2ParticipantClient: signalv2ParticipantClient,
	}
}

func (s *RTCv2Service) SetupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST "+cRTCv2Path, s.handlePost)
	mux.HandleFunc("GET "+cRTCv2ValidatePath, s.validate)
	mux.HandleFunc("PATCH "+cRTCv2ParticipantIDPath, s.handleParticipantPatch)
	mux.HandleFunc("DELETE "+cRTCv2ParticipantIDPath, s.handleParticipantDelete)
}

func (s *RTCv2Service) validateInternal(
	lgr logger.Logger,
	r *http.Request,
	wireMessage *livekit.Signalv2WireMessage,
) (livekit.RoomName, livekit.ParticipantIdentity, *rpc.RelaySignalv2ConnectRequest, int, error) {
	connectRequest := signalling.GetConnectRequest(wireMessage)
	if connectRequest == nil {
		return "", "", nil, http.StatusBadRequest, ErrNoConnectRequest
	}

	params := ValidateConnectRequestParams{
		metadata:   connectRequest.Metadata,
		attributes: connectRequest.ParticipantAttributes,
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
		return "", "", nil, code, err
	}

	grantsJson, err := json.Marshal(res.grants)
	if err != nil {
		return "", "", nil, http.StatusInternalServerError, err
	}

	AugmentClientInfo(connectRequest.ClientInfo, r)

	return res.roomName,
		livekit.ParticipantIdentity(res.grants.Identity),
		&rpc.RelaySignalv2ConnectRequest{
			GrantsJson:  string(grantsJson),
			CreateRoom:  res.createRoomRequest,
			WireMessage: wireMessage,
		},
		code,
		err
}

func (s *RTCv2Service) handlePost(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Content-type") != "application/x-protobuf" {
		HandleErrorJson(w, r, http.StatusBadRequest, fmt.Errorf("unsupported content-type: %s", r.Header.Get("Content-type")))
		return
	}

	wireMessage, err := getWireMessage(r)
	if err != nil {
		HandleErrorJson(w, r, http.StatusBadRequest, fmt.Errorf("could not get wire message: %w", err))
		return
	}

	// only connect requests should be coming in here and there should not be fragments
	roomName, participantIdentity, rscr, code, err := s.validateInternal(
		utils.GetLogger(r.Context()),
		r,
		wireMessage,
	)
	if err != nil {
		HandleErrorJson(w, r, code, err)
		return
	}

	if err := s.roomAllocator.SelectRoomNode(r.Context(), roomName, ""); err != nil {
		HandleErrorJson(w, r, http.StatusInternalServerError, err)
		return
	}

	resp, err := s.router.HandleParticipantConnectRequest(r.Context(), roomName, participantIdentity, rscr)
	if err != nil {
		HandleErrorJson(w, r, http.StatusInternalServerError, err)
		return
	}

	connectResponse := signalling.GetConnectResponse(resp.WireMessage)
	if connectResponse == nil {
		HandleErrorJson(w, r, http.StatusInternalServerError, ErrNoConnectResponse)
		return
	}

	marshalled, err := proto.Marshal(resp.WireMessage)
	if err != nil {
		HandleErrorJson(w, r, http.StatusInternalServerError, err)
		return
	}

	w.Header().Add("Content-type", "application/x-protobuf")
	w.Write(marshalled)

	logger.Debugw(
		"connect response",
		"room", roomName,
		"roomID", connectResponse.Room.Sid, // SIGNALLING-V2-TODO: roomID may not be resolved
		"participant", participantIdentity,
		"pID", connectResponse.Participant.Sid,
		"wireMessage", logger.Proto(resp.WireMessage),
	)

	w.WriteHeader(http.StatusOK)
}

func (s *RTCv2Service) validate(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Content-type") != "application/x-protobuf" {
		HandleErrorJson(w, r, http.StatusBadRequest, fmt.Errorf("unsupported content-type: %s", r.Header.Get("Content-type")))
		return
	}

	wireMessage, err := getWireMessage(r)
	if err != nil {
		HandleErrorJson(w, r, http.StatusBadRequest, fmt.Errorf("could not get wire message: %w", err))
		return
	}

	_, _, _, code, err := s.validateInternal(utils.GetLogger(r.Context()), r, wireMessage)
	if err != nil {
		HandleErrorJson(w, r, code, err)
		return
	}

	_, _ = w.Write([]byte("success"))
	w.WriteHeader(http.StatusOK)
}

func (s *RTCv2Service) handleParticipantPatch(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Content-type") != "application/x-protobuf" {
		HandleErrorJson(w, r, http.StatusBadRequest, fmt.Errorf("unsupported content-type: %s", r.Header.Get("Content-type")))
		return
	}

	roomName, participantIdentity, pID, code, err := getParams(r)
	if err != nil {
		HandleErrorJson(w, r, code, err)
		return
	}

	wireMessage, err := getWireMessage(r)
	if err != nil {
		HandleErrorJson(w, r, http.StatusBadRequest, fmt.Errorf("could not get wire message: %w", err))
		return
	}

	res, err := s.signalv2ParticipantClient.RelaySignalv2Participant(
		r.Context(),
		s.topicFormatter.ParticipantTopic(r.Context(), roomName, participantIdentity),
		&rpc.RelaySignalv2ParticipantRequest{
			Room:                string(roomName),
			ParticipantIdentity: string(participantIdentity),
			ParticipantId:       string(pID),
			WireMessage:         wireMessage,
		},
	)
	if err != nil {
		var pe psrpc.Error
		if errors.As(err, &pe) {
			switch pe.Code() {
			case psrpc.NotFound:
				HandleErrorJson(w, r, http.StatusNotFound, errors.New(pe.Error()))

			case psrpc.InvalidArgument:
				HandleErrorJson(w, r, http.StatusBadRequest, errors.New(pe.Error()))
			default:
				HandleErrorJson(w, r, http.StatusInternalServerError, errors.New(pe.Error()))
			}
		} else {
			HandleErrorJson(w, r, http.StatusInternalServerError, nil)
		}
		return
	}

	logger.Debugw(
		"participant response",
		"room", roomName,
		"participant", participantIdentity,
		"pID", pID,
		"participantResponse", logger.Proto(res),
	)

	marshalled, err := proto.Marshal(res.WireMessage)
	if err != nil {
		HandleErrorJson(w, r, http.StatusInternalServerError, err)
		return
	}

	w.Header().Add("Content-type", "application/x-protobuf")
	w.Write(marshalled)

	w.WriteHeader(http.StatusOK)
}

func (s *RTCv2Service) handleParticipantDelete(w http.ResponseWriter, r *http.Request) {
	claims := GetGrants(r.Context())
	if claims == nil || claims.Video == nil {
		HandleErrorJson(w, r, http.StatusUnauthorized, rtc.ErrPermissionDenied)
		return
	}

	roomName, participantIdentity, pID, code, err := getParams(r)
	if err != nil {
		HandleErrorJson(w, r, code, err)
		return
	}

	_, err = s.signalv2ParticipantClient.RelaySignalv2ParticipantDeleteSession(
		r.Context(),
		s.topicFormatter.ParticipantTopic(r.Context(), roomName, participantIdentity),
		&rpc.RelaySignalv2ParticipantDeleteSessionRequest{
			Room:                string(roomName),
			ParticipantIdentity: string(participantIdentity),
			ParticipantId:       string(pID),
		},
	)
	if err != nil {
		HandleErrorJson(w, r, http.StatusBadRequest, err)
		return
	}

	logger.Debugw(
		"participant deleted",
		"room", roomName,
		"participant", participantIdentity,
		"pID", pID,
	)

	w.WriteHeader(http.StatusOK)
}

// ---------------------------------------

func getWireMessage(r *http.Request) (*livekit.Signalv2WireMessage, error) {
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}

	wireMessage := &livekit.Signalv2WireMessage{}
	err = proto.Unmarshal(body, wireMessage)
	if err != nil {
		return nil, err
	}

	return wireMessage, nil
}

func getParams(r *http.Request) (livekit.RoomName, livekit.ParticipantIdentity, livekit.ParticipantID, int, error) {
	claims := GetGrants(r.Context())
	if claims == nil || claims.Video == nil {
		return "", "", "", http.StatusUnauthorized, rtc.ErrPermissionDenied
	}

	roomName, err := EnsureJoinPermission(r.Context())
	if err != nil {
		return "", "", "", http.StatusUnauthorized, err
	}
	if roomName == "" {
		return "", "", "", http.StatusUnauthorized, ErrNoRoomName
	}

	participantIdentity := livekit.ParticipantIdentity(claims.Identity)
	if participantIdentity == "" {
		return "", "", "", http.StatusUnauthorized, ErrIdentityEmpty
	}

	pID := livekit.ParticipantID(r.PathValue("participant_id"))
	if pID == "" {
		return "", "", "", http.StatusBadRequest, ErrParticipantSidEmpty
	}

	return roomName, participantIdentity, pID, http.StatusOK, nil
}
