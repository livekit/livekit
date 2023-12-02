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

package routing

import (
	"errors"
)

var (
	ErrNotFound             = errors.New("could not find object")
	ErrIPNotSet             = errors.New("ip address is required and not set")
	ErrHandlerNotDefined    = errors.New("handler not defined")
	ErrIncorrectRTCNode     = errors.New("current node isn't the RTC node for the room")
	ErrNodeNotFound         = errors.New("could not locate the node")
	ErrNodeLimitReached     = errors.New("reached configured limit for node")
	ErrInvalidRouterMessage = errors.New("invalid router message")
	ErrChannelClosed        = errors.New("channel closed")
	ErrChannelFull          = errors.New("channel is full")

	// errors when starting signal connection
	ErrRequestChannelClosed       = errors.New("request channel closed")
	ErrCouldNotMigrateParticipant = errors.New("could not migrate participant")
	ErrClientInfoNotSet           = errors.New("client info not set")
)
