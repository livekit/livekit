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

package rtc

import (
	"errors"
)

var (
	ErrRoomClosed               = errors.New("room has already closed")
	ErrParticipantSessionClosed = errors.New("participant session is already closed")
	ErrPermissionDenied         = errors.New("no permissions to access the room")
	ErrMaxParticipantsExceeded  = errors.New("room has exceeded its max participants")
	ErrLimitExceeded            = errors.New("node has exceeded its configured limit")
	ErrAlreadyJoined            = errors.New("a participant with the same identity is already in the room")
	ErrDataChannelUnavailable   = errors.New("data channel is not available")
	ErrDataChannelBufferFull    = errors.New("data channel buffer is full")
	ErrTransportFailure         = errors.New("transport failure")
	ErrEmptyIdentity            = errors.New("participant identity cannot be empty")
	ErrEmptyParticipantID       = errors.New("participant ID cannot be empty")
	ErrMissingGrants            = errors.New("VideoGrant is missing")
	ErrInternalError            = errors.New("internal error")

	// Track subscription related
	ErrNoTrackPermission         = errors.New("participant is not allowed to subscribe to this track")
	ErrNoSubscribePermission     = errors.New("participant is not given permission to subscribe to tracks")
	ErrTrackNotFound             = errors.New("track cannot be found")
	ErrTrackNotBound             = errors.New("track not bound")
	ErrSubscriptionLimitExceeded = errors.New("participant has exceeded its subscription limit")

	ErrNoSubscribeMetricsPermission = errors.New("participant is not given permission to subscribe to metrics")
)
