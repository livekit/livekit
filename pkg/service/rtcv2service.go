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
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"
	"github.com/livekit/psrpc/pkg/middleware"
	"google.golang.org/protobuf/proto"
)

const (
	cRTCv2Path = "/rtc/v2"
)

type RTCv2Service struct {
	http.Handler

	limits        config.LimitConfig
	router        routing.Router
	roomAllocator RoomAllocator
	client        rpc.TypedSignalv2Client
	/* RAJA-TODO
	client            rpc.RTCv2ServiceClient[livekit.NodeID]
	topicFormatter    rpc.TopicFormatter
	participantClient rpc.TypedRTCv2ServiceParticipantClient
	*/
}

func NewRTCv2Service(
	nodeID livekit.NodeID,
	config *config.Config,
	router routing.Router,
	roomAllocator RoomAllocator,
	bus psrpc.MessageBus,
	/* RAJA-TODO
	   clientParams rpc.ClientParams,
	   topicFormatter rpc.TopicFormatter,
	   participantClient rpc.TypedRTCv2ServiceParticipantClient,
	*/
) (*RTCv2Service, error) {
	client, err := rpc.NewTypedSignalv2Client(
		nodeID,
		bus,
		middleware.WithClientMetrics(rpc.PSRPCMetricsObserver{}),
	)
	if err != nil {
		return nil, err
	}

	return &RTCv2Service{
		limits:        config.Limit,
		router:        router,
		roomAllocator: roomAllocator,
		client:        client,
		/* RAJA-TODO
		topicFormatter:    topicFormatter,
		participantClient: participantClient,
		*/
	}, nil
}

func (s *RTCv2Service) SetupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST "+cRTCv2Path, s.handlePost)
}

func (s *RTCv2Service) validateInternal(
	lgr logger.Logger,
	r *http.Request,
	connectRequest *livekit.ConnectRequest,
) (livekit.RoomName, *rpc.RelaySignalv2ConnectRequest, int, error) {
	params := ValidateConnectRequestParams{
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
		return res.roomName, nil, code, err
	}

	grantsJson, err := json.Marshal(res.grants)
	if err != nil {
		return res.roomName, nil, http.StatusInternalServerError, err
	}

	AugmentClientInfo(connectRequest.ClientInfo, r)

	return res.roomName, &rpc.RelaySignalv2ConnectRequest{
		GrantsJson:     string(grantsJson),
		CreateRoom:     res.createRoomRequest,
		ConnectRequest: connectRequest,
	}, code, err
}

func (s *RTCv2Service) handlePost(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Content-type") != "application/x-protobuf" {
		HandleErrorJson(w, r, http.StatusBadRequest, fmt.Errorf("unsupported content-type: %s", r.Header.Get("Content-type")))
		return
	}

	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		HandleErrorJson(w, r, http.StatusBadRequest, fmt.Errorf("could not read request body: %s", err))
		return
	}

	clientMessage := &livekit.Signalv2ClientMessage{}
	err = proto.Unmarshal(body, clientMessage)
	if err != nil {
		HandleErrorJson(w, r, http.StatusBadRequest, fmt.Errorf("could not unmarshal request: %s", err))
		return
	}

	switch msg := clientMessage.GetMessage().(type) {
	case *livekit.Signalv2ClientMessage_ConnectRequest:
		roomName, rscr, code, err := s.validateInternal(logger.GetLogger(), r, msg.ConnectRequest)
		if err != nil {
			HandleErrorJson(w, r, code, err)
			return
		}

		if err := s.roomAllocator.SelectRoomNode(r.Context(), roomName, ""); err != nil {
			HandleErrorJson(w, r, http.StatusInternalServerError, err)
			return
		}

		node, err := s.router.GetNodeForRoom(r.Context(), roomName)
		if err != nil {
			HandleErrorJson(w, r, http.StatusInternalServerError, err)
			return
		}

		resp, err := s.client.RelaySignalv2Connect(context.Background(), livekit.NodeID(node.Id), rscr)
		if err != nil {
			HandleErrorJson(w, r, http.StatusInternalServerError, err)
			return
		}

		serverMessage := &livekit.Signalv2ServerMessage{
			Message: &livekit.Signalv2ServerMessage_ConnectResponse{
				ConnectResponse: resp.ConnectResponse,
			},
		}
		marshalled, err := proto.Marshal(serverMessage)
		if err != nil {
			HandleErrorJson(w, r, http.StatusInternalServerError, err)
			return
		}

		w.Header().Add("Content-type", "application/x-protobuf")
		w.Write(marshalled)

	case *livekit.Signalv2ClientMessage_Envelope:
		for _, cm := range msg.Envelope.ClientMessages {
			switch oneOf := cm.GetMessage().(type) {
			case *livekit.Signalv2ClientMessage_ConnectRequest:
				logger.Errorw("should not get ConnectRequest in envelope", nil)
			default:
				logger.Debugw("unhandled message", "message", oneOf)
			}
		}
	}

	w.WriteHeader(http.StatusOK)
}
