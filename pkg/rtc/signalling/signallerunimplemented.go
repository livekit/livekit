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

package signalling

import (
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc/types"

	"google.golang.org/protobuf/proto"
)

var _ ParticipantSignaller = (*signallerUnimplemented)(nil)

type signallerUnimplemented struct{}

func (u *signallerUnimplemented) SetResponseSink(sink routing.MessageSink) {}

func (u *signallerUnimplemented) GetResponseSink() routing.MessageSink {
	return nil
}

func (u *signallerUnimplemented) CloseSignalConnection(reason types.SignallingCloseReason) {}

func (u *signallerUnimplemented) WriteMessage(msg proto.Message) error {
	return nil
}
