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
	"errors"
	"fmt"
	"net/http"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/routing/selector"
	"github.com/livekit/livekit-server/pkg/rtc"
)

type ValidateConnectRequestParams struct {
	roomName             livekit.RoomName
	reconnect            bool
	reconnectReason      livekit.ReconnectReason
	autoSubscribe        bool
	publish              string
	adaptiveStream       bool
	participantID        livekit.ParticipantID
	subscriberAllowPause *bool
	disableICELite       bool
	attributes           map[string]string
}

func ValidateConnectRequest(
	r *http.Request,
	limitConfig config.LimitConfig,
	params ValidateConnectRequestParams,
	router routing.MessageRouter,
	roomAllocator RoomAllocator,
) (livekit.RoomName, routing.ParticipantInit, int, error) {
	var pi routing.ParticipantInit

	// require a claim
	claims := GetGrants(r.Context())
	if claims == nil || claims.Video == nil {
		return "", pi, http.StatusUnauthorized, rtc.ErrPermissionDenied
	}

	roomNameInToken, err := EnsureJoinPermission(r.Context())
	if err != nil {
		return "", pi, http.StatusUnauthorized, err
	}

	if claims.Identity == "" {
		return "", pi, http.StatusBadRequest, ErrIdentityEmpty
	}
	if !limitConfig.CheckParticipantIdentityLength(claims.Identity) {
		return "", pi, http.StatusBadRequest, fmt.Errorf("%w: max length %d", ErrParticipantIdentityExceedsLimits, limitConfig.MaxParticipantIdentityLength)
	}

	if claims.RoomConfig != nil {
		if err := claims.RoomConfig.CheckCredentials(); err != nil {
			logger.Warnw("credentials found in token", nil)
			// TODO(dz): in a future version, we'll reject these connections
		}
	}

	roomName := params.roomName
	if roomNameInToken != "" {
		roomName = roomNameInToken
	}
	if roomName == "" {
		return "", pi, http.StatusBadRequest, ErrNoRoomName
	}
	if !limitConfig.CheckRoomNameLength(string(roomName)) {
		return "", pi, http.StatusBadRequest, fmt.Errorf("%w: max length %d", ErrRoomNameExceedsLimits, limitConfig.MaxRoomNameLength)
	}

	// this is new connection for existing participant -  with publish only permissions
	if params.publish != "" {
		// Make sure grant has GetCanPublish set,
		if !claims.Video.GetCanPublish() {
			return "", routing.ParticipantInit{}, http.StatusUnauthorized, rtc.ErrPermissionDenied
		}
		// Make sure by default subscribe is off
		claims.Video.SetCanSubscribe(false)
		claims.Identity += "#" + params.publish
	}

	// room allocator validations
	err = roomAllocator.ValidateCreateRoom(r.Context(), roomName)
	if err != nil {
		if errors.Is(err, ErrRoomNotFound) {
			return "", pi, http.StatusNotFound, err
		} else {
			return "", pi, http.StatusInternalServerError, err
		}
	}

	region := ""
	if router, ok := router.(routing.Router); ok {
		region = router.GetRegion()
		if foundNode, err := router.GetNodeForRoom(r.Context(), roomName); err == nil {
			if selector.LimitsReached(limitConfig, foundNode.Stats) {
				return "", pi, http.StatusServiceUnavailable, rtc.ErrLimitExceeded
			}
		}
	}

	createRequest := &livekit.CreateRoomRequest{
		Name:       string(roomName),
		RoomPreset: claims.RoomPreset,
	}
	SetRoomConfiguration(createRequest, claims.GetRoomConfiguration())

	// Add extra attributes to the participant
	if len(params.attributes) != 0 {
		// Make sure grant has GetCanUpdateOwnMetadata set
		if !claims.Video.GetCanUpdateOwnMetadata() {
			return "", routing.ParticipantInit{}, http.StatusUnauthorized, rtc.ErrPermissionDenied
		}
		if claims.Attributes == nil {
			claims.Attributes = make(map[string]string, len(params.attributes))
		}
		for k, v := range params.attributes {
			if v == "" {
				continue // do not allow deleting existing attributes
			}
			claims.Attributes[k] = v
		}
	}

	pi = routing.ParticipantInit{
		Reconnect:            params.reconnect,
		ReconnectReason:      params.reconnectReason,
		Identity:             livekit.ParticipantIdentity(claims.Identity),
		Name:                 livekit.ParticipantName(claims.Name),
		Client:               ParseClientInfo(r),
		Grants:               claims,
		Region:               region,
		CreateRoom:           createRequest,
		AutoSubscribe:        params.autoSubscribe,
		AdaptiveStream:       params.adaptiveStream,
		SubscriberAllowPause: params.subscriberAllowPause,
		DisableICELite:       params.disableICELite,
	}
	if pi.Reconnect {
		pi.ID = livekit.ParticipantID(params.participantID)
	}

	return roomName, pi, http.StatusOK, nil
}
