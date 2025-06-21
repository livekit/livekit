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
	"net/url"
	"strings"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	sutils "github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/utils/guid"
	"github.com/livekit/psrpc"
	"github.com/pion/webrtc/v4"
	"github.com/tomnomnom/linkheader"
)

const (
	cParticipantPath   = "/whip/v1"
	cParticipantIDPath = "/whip/v1/{participant_id}"
)

type RTCRestService struct {
	http.Handler

	config            *config.Config
	router            routing.Router
	roomAllocator     RoomAllocator
	client            rpc.RTCRestClient[livekit.NodeID]
	topicFormatter    rpc.TopicFormatter
	participantClient rpc.TypedRTCRestParticipantClient
}

func NewRTCRestService(
	config *config.Config,
	router routing.Router,
	roomAllocator RoomAllocator,
	clientParams rpc.ClientParams,
	topicFormatter rpc.TopicFormatter,
	participantClient rpc.TypedRTCRestParticipantClient,
) (*RTCRestService, error) {
	client, err := rpc.NewRTCRestClient[livekit.NodeID](clientParams.Args())
	if err != nil {
		return nil, err
	}

	return &RTCRestService{
		config:            config,
		router:            router,
		roomAllocator:     roomAllocator,
		client:            client,
		topicFormatter:    topicFormatter,
		participantClient: participantClient,
	}, nil
}

func (s *RTCRestService) SetupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+cParticipantPath, s.handleGet)
	mux.HandleFunc("OPTIONS "+cParticipantPath, s.handleOptions)
	mux.HandleFunc("POST "+cParticipantPath, s.handleCreate)
	mux.HandleFunc("GET "+cParticipantIDPath, s.handleParticipantGet)
	mux.HandleFunc("PATCH "+cParticipantIDPath, s.handleParticipantPatch)
	mux.HandleFunc("DELETE "+cParticipantIDPath, s.handleParticipantDelete)
}

func (s *RTCRestService) handleGet(w http.ResponseWriter, r *http.Request) {
	// https:/www.rfc-editor.org/rfc/rfc9725.html#name-http-usage
	w.WriteHeader(http.StatusNoContent)
}

func (s *RTCRestService) handleOptions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "*")
	w.Header().Set("Access-Control-Allow-Methods", "PATCH, OPTIONS, GET, POST, DELETE")
	w.Header().Set("Access-Control-Expose-Headers", "*")

	w.WriteHeader(http.StatusOK)

	// According to https://www.rfc-editor.org/rfc/rfc9725.html#name-stun-turn-server-configurat,
	// ICE servers can be returned in OPTIONS response, but not recommended.
	//
	// Supporting that here is tricky. This would have to get region settings like the
	// session CREATE POST request and send a request to get ICE servers from a
	// region + media node that is selected. The issue is that a subsequent POST,
	// although unlikely, may end up in a different region. Media node in one region and
	// TURN in another region, although shuttling media across regions,  should still work.
	// But, as this is not a recommended way, not supporting it.
}

type createRequest struct {
	RoomName                        livekit.RoomName
	ParticipantInit                 routing.ParticipantInit
	ClientIP                        string
	OfferSDP                        string
	SubscribedParticipantTrackNames map[string][]string
}

func (s *RTCRestService) validateCreate(r *http.Request) (*createRequest, int, error) {
	claims := GetGrants(r.Context())
	if claims == nil || claims.Video == nil {
		return nil, http.StatusUnauthorized, rtc.ErrPermissionDenied
	}

	roomName, err := EnsureJoinPermission(r.Context())
	if err != nil {
		return nil, http.StatusUnauthorized, err
	}
	if roomName == "" {
		return nil, http.StatusUnauthorized, errors.New("room name cannot be empty")
	}
	if !s.config.Limit.CheckRoomNameLength(string(roomName)) {
		return nil, http.StatusBadRequest, fmt.Errorf("%w: max length %d", ErrRoomNameExceedsLimits, s.config.Limit.MaxRoomNameLength)
	}

	if claims.Identity == "" {
		return nil, http.StatusBadRequest, ErrIdentityEmpty
	}
	if !s.config.Limit.CheckParticipantIdentityLength(claims.Identity) {
		return nil, http.StatusBadRequest, fmt.Errorf("%w: max length %d", ErrParticipantIdentityExceedsLimits, s.config.Limit.MaxParticipantIdentityLength)
	}

	var clientInfo struct {
		ClientIP                        string              `json:"clientIp"`
		SubscribedParticipantTrackNames map[string][]string `json:"subscribedParticipantTrackNames"`
	}
	clientInfoHeader := r.Header.Get("X-LiveKit-ClientInfo")
	if clientInfoHeader != "" {
		if err := json.NewDecoder(strings.NewReader(clientInfoHeader)).Decode(&clientInfo); err != nil {
			return nil, http.StatusBadRequest, fmt.Errorf("malformed json in client info header: %s", err)
		}
	}

	offerSDPBytes, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("body does not have SDP offer: %s", err)
	}
	if len(offerSDPBytes) == 0 {
		return nil, http.StatusBadRequest, errors.New("body does not have SDP offer")
	}
	offerSDP := string(offerSDPBytes)
	sd := &webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  offerSDP,
	}
	_, err = sd.Unmarshal()
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("malformed SDP offer: %s", err)
	}

	ci := ParseClientInfo(r)
	if ci.Protocol == 0 {
		// if no client info available (which will be mostly the case with WHIP clients), at least set protocol
		ci.Protocol = types.CurrentProtocol
	}

	pi := routing.ParticipantInit{
		Identity:      livekit.ParticipantIdentity(claims.Identity),
		Name:          livekit.ParticipantName(claims.Name),
		AutoSubscribe: true,
		Client:        ci,
		Grants:        claims,
		CreateRoom: &livekit.CreateRoomRequest{
			Name:       string(roomName),
			RoomPreset: claims.RoomPreset,
		},
		AdaptiveStream: false,
		DisableICELite: true,
	}
	SetRoomConfiguration(pi.CreateRoom, claims.GetRoomConfiguration())

	return &createRequest{
		roomName,
		pi,
		clientInfo.ClientIP,
		offerSDP,
		clientInfo.SubscribedParticipantTrackNames,
	}, http.StatusOK, nil
}

func (s *RTCRestService) handleCreate(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Content-type") != "application/sdp" {
		handleError("Create", w, r, http.StatusBadRequest, fmt.Errorf("unsupported content-type: %s", r.Header.Get("Content-type")))
		return
	}

	w.Header().Add("Content-type", "application/sdp")

	req, status, err := s.validateCreate(r)
	if err != nil {
		handleError("Create", w, r, status, err)
		return
	}

	if err := s.roomAllocator.SelectRoomNode(r.Context(), req.RoomName, ""); err != nil {
		handleError("Create", w, r, http.StatusInternalServerError, err)
		return
	}

	rtcNode, err := s.router.GetNodeForRoom(r.Context(), req.RoomName)
	if err != nil {
		handleError("Create", w, r, http.StatusInternalServerError, err)
		return
	}

	connID := livekit.ConnectionID(guid.New("CO_"))
	starSession, err := req.ParticipantInit.ToStartSession(req.RoomName, connID)
	if err != nil {
		handleError("Create", w, r, http.StatusInternalServerError, err)
		return
	}

	subscribedParticipantTracks := map[string]*rpc.RTCRestCreateRequest_TrackList{}
	for identity, trackNames := range req.SubscribedParticipantTrackNames {
		subscribedParticipantTracks[identity] = &rpc.RTCRestCreateRequest_TrackList{
			TrackNames: trackNames,
		}
	}

	res, err := s.client.Create(r.Context(), livekit.NodeID(rtcNode.Id), &rpc.RTCRestCreateRequest{
		OfferSdp:                    req.OfferSDP,
		StartSession:                starSession,
		SubscribedParticipantTracks: subscribedParticipantTracks,
	})
	if err != nil {
		handleError("Create", w, r, http.StatusServiceUnavailable, err)
		return
	}

	// created resource sent in Location header:
	// https://www.rfc-editor.org/rfc/rfc9725.html#name-ingest-session-setup
	// using relative location
	w.Header().Add("Location", fmt.Sprintf("%s/%s", cParticipantPath, res.ParticipantId))

	// ICE servers as Link header(s):
	// https://www.rfc-editor.org/rfc/rfc9725.html#name-stun-turn-server-configurat
	var iceServerLinks []*linkheader.Link
	for _, iceServer := range res.IceServers {
		for _, iceURL := range iceServer.Urls {
			iceServerLink := &linkheader.Link{
				URL:    url.PathEscape(iceURL),
				Rel:    "ice-server",
				Params: map[string]string{},
			}
			if iceServer.Username != "" {
				iceServerLink.Params["username"] = iceServer.Username
			}
			if iceServer.Credential != "" {
				iceServerLink.Params["credential"] = iceServer.Credential
			}

			iceServerLinks = append(iceServerLinks, iceServerLink)
		}
	}
	for _, iceServerLink := range iceServerLinks {
		w.Header().Add("Link", iceServerLink.String())
	}

	// To support ICE Trickle/Restart, HTTP PATCH should have an ETag
	// send ICE session ID (ICE ufrag is used as ID) in ETag header
	// https://www.rfc-editor.org/rfc/rfc9725.html#name-http-patch-request-usage
	if res.IceSessionId != "" {
		w.Header().Add("ETag", res.IceSessionId)
	}

	// 201 Status Created
	w.WriteHeader(http.StatusCreated)

	// SDP answer in the response body
	w.Write([]byte(res.AnswerSdp))

	sutils.GetLogger(r.Context()).Infow(
		"API RTCRest.Create",
		"connID", connID,
		"participant", req.ParticipantInit.Identity,
		"room", req.RoomName,
		"status", http.StatusCreated,
		"response", logger.Proto(res),
	)
	return
}

func (s *RTCRestService) handleParticipantGet(w http.ResponseWriter, r *http.Request) {
	// https:/www.rfc-editor.org/rfc/rfc9725.html#name-http-usage
	w.WriteHeader(http.StatusNoContent)
}

func (s *RTCRestService) iceTrickle(
	w http.ResponseWriter,
	r *http.Request,
	roomName livekit.RoomName,
	participantIdentity livekit.ParticipantIdentity,
	pID livekit.ParticipantID,
	iceSessionID string,
	sdpFragment string,
) {
	_, err := s.participantClient.ICETrickle(
		r.Context(),
		s.topicFormatter.ParticipantTopic(r.Context(), roomName, participantIdentity),
		&rpc.RTCRestParticipantICETrickleRequest{
			Room:                string(roomName),
			ParticipantIdentity: string(participantIdentity),
			ParticipantId:       string(pID),
			IceSessionId:        iceSessionID,
			SdpFragment:         sdpFragment,
		},
	)
	if err != nil {
		var pe psrpc.Error
		if errors.As(err, &pe) {
			switch pe.Code() {
			case psrpc.NotFound:
				handleError("Patch", w, r, http.StatusNotFound, errors.New(pe.Error()))

			case psrpc.InvalidArgument:
				switch pe.Error() {
				case rtc.ErrInvalidSDPFragment.Error(), rtc.ErrMidMismatch.Error(), rtc.ErrICECredentialMismatch.Error():
					handleError("Patch", w, r, http.StatusBadRequest, errors.New(pe.Error()))
				default:
					handleError("Patch", w, r, http.StatusInternalServerError, errors.New(pe.Error()))
				}
			default:
				handleError("Patch", w, r, http.StatusInternalServerError, errors.New(pe.Error()))
			}
		} else {
			handleError("Patch", w, r, http.StatusInternalServerError, nil)
		}
		return
	}
	sutils.GetLogger(r.Context()).Infow(
		"API RTCRest.Patch",
		"method", "ice-trickle",
		"room", roomName,
		"participant", participantIdentity,
		"pID", pID,
		"sdpFragment", sdpFragment,
		"status", http.StatusNoContent,
	)
	w.WriteHeader(http.StatusNoContent)
}

func (s *RTCRestService) iceRestart(
	w http.ResponseWriter,
	r *http.Request,
	roomName livekit.RoomName,
	participantIdentity livekit.ParticipantIdentity,
	pID livekit.ParticipantID,
	sdpFragment string,
) {
	res, err := s.participantClient.ICERestart(
		r.Context(),
		s.topicFormatter.ParticipantTopic(r.Context(), roomName, participantIdentity),
		&rpc.RTCRestParticipantICERestartRequest{
			Room:                string(roomName),
			ParticipantIdentity: string(participantIdentity),
			ParticipantId:       string(pID),
			SdpFragment:         sdpFragment,
		},
	)
	if err != nil {
		var pe psrpc.Error
		if errors.As(err, &pe) {
			switch pe.Code() {
			case psrpc.NotFound:
				handleError("Patch", w, r, http.StatusNotFound, errors.New(pe.Error()))

			case psrpc.InvalidArgument:
				switch pe.Error() {
				case rtc.ErrInvalidSDPFragment.Error():
					handleError("Patch", w, r, http.StatusBadRequest, errors.New(pe.Error()))
				default:
					handleError("Patch", w, r, http.StatusInternalServerError, errors.New(pe.Error()))
				}
			default:
				handleError("Patch", w, r, http.StatusInternalServerError, errors.New(pe.Error()))
			}
		} else {
			handleError("Patch", w, r, http.StatusInternalServerError, nil)
		}
		return
	}
	sutils.GetLogger(r.Context()).Infow(
		"API RTCRest.Patch",
		"method", "ice-restart",
		"room", roomName,
		"participant", participantIdentity,
		"pID", pID,
		"sdpFragment", sdpFragment,
		"status", http.StatusNoContent,
		"res", logger.Proto(res),
	)
	if res.IceSessionId != "" {
		w.Header().Add("ETag", res.IceSessionId)
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(res.SdpFragment))
}

func (s *RTCRestService) handleParticipantPatch(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Content-type") != "application/trickle-ice-sdpfrag" {
		handleError("Patch", w, r, http.StatusBadRequest, fmt.Errorf("unsupported content-type: %s", r.Header.Get("Content-type")))
		return
	}

	w.Header().Add("Content-type", "application/trickle-ice-sdpfrag")

	// https://www.rfc-editor.org/rfc/rfc9725.html#name-http-patch-request-usage
	ifMatch := r.Header.Get("If-Match")
	if ifMatch == "" {
		handleError("Patch", w, r, http.StatusPreconditionRequired, errors.New("missing entity tag"))
		return
	}

	claims := GetGrants(r.Context())
	if claims == nil || claims.Video == nil {
		handleError("Patch", w, r, http.StatusUnauthorized, rtc.ErrPermissionDenied)
		return
	}

	roomName, err := EnsureJoinPermission(r.Context())
	if err != nil {
		handleError("Patch", w, r, http.StatusUnauthorized, err)
		return
	}
	if roomName == "" {
		handleError("Patch", w, r, http.StatusUnauthorized, errors.New("room name cannot be empty"))
		return
	}
	if claims.Identity == "" {
		handleError("Patch", w, r, http.StatusUnauthorized, errors.New("participant identity cannot be empty"))
		return
	}
	pID := livekit.ParticipantID(r.PathValue("participant_id"))
	if pID == "" {
		handleError("Patch", w, r, http.StatusUnauthorized, errors.New("participant ID cannot be empty"))
		return
	}

	sdpFragmentBytes, err := ioutil.ReadAll(r.Body)
	if err != nil {
		handleError("Patch", w, r, http.StatusBadRequest, fmt.Errorf("body does not have SDP fragment: %s", err))
	}
	sdpFragment := string(sdpFragmentBytes)

	if ifMatch == "*" {
		s.iceRestart(w, r, roomName, livekit.ParticipantIdentity(claims.Identity), pID, sdpFragment)
	} else {
		s.iceTrickle(w, r, roomName, livekit.ParticipantIdentity(claims.Identity), pID, ifMatch, sdpFragment)
	}
}

func (s *RTCRestService) handleParticipantDelete(w http.ResponseWriter, r *http.Request) {
	claims := GetGrants(r.Context())
	if claims == nil || claims.Video == nil {
		handleError("Delete", w, r, http.StatusUnauthorized, rtc.ErrPermissionDenied)
		return
	}

	roomName, err := EnsureJoinPermission(r.Context())
	if err != nil {
		handleError("Delete", w, r, http.StatusUnauthorized, err)
		return
	}
	if roomName == "" {
		handleError("Delete", w, r, http.StatusUnauthorized, errors.New("room name cannot be empty"))
		return
	}
	if claims.Identity == "" {
		handleError("Delete", w, r, http.StatusUnauthorized, errors.New("participant identity cannot be empty"))
		return
	}

	_, err = s.participantClient.DeleteSession(
		r.Context(),
		s.topicFormatter.ParticipantTopic(r.Context(), roomName, livekit.ParticipantIdentity(claims.Identity)),
		&rpc.RTCRestParticipantDeleteSessionRequest{
			Room:                string(roomName),
			ParticipantIdentity: claims.Identity,
			ParticipantId:       r.PathValue("participant_id"),
		},
	)
	if err != nil {
		handleError("Delete", w, r, http.StatusNotFound, err)
		return
	}

	sutils.GetLogger(r.Context()).Infow(
		"API RTCRest.Delete",
		"participant", claims.Identity,
		"pID", r.PathValue("participant_id"),
		"room", roomName,
		"status", http.StatusOK,
	)
	w.WriteHeader(http.StatusOK)
}

func handleError(method string, w http.ResponseWriter, r *http.Request, status int, err error) {
	sutils.GetLogger(r.Context()).Warnw(
		fmt.Sprintf("API RTCRest.%s", method), err,
		"status", status,
	)
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
	}{
		Error: err.Error(),
	})
}
