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
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/pion/webrtc/v4"

	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/sfu/datachannel"

	"google.golang.org/protobuf/proto"
)

var _ routing.MessageSink = (*dataChannelMessageSink)(nil)

type DataChannelMessageSinkParams struct {
	Logger      logger.Logger
	DataChannel *datachannel.DataChannelWriter[*webrtc.DataChannel]
}

type dataChannelMessageSink struct {
	params DataChannelMessageSinkParams
}

func NewDataChannelMessageSink(params DataChannelMessageSinkParams) routing.MessageSink {
	return &dataChannelMessageSink{
		params: params,
	}
}

func (d *dataChannelMessageSink) WriteMessage(msg proto.Message) error {
	if msg == nil {
		return nil
	}

	protoMsg, err := proto.Marshal(msg)
	if err != nil {
		d.params.Logger.Errorw("could not marshal message", err)
		return err
	}

	if _, err := d.params.DataChannel.Write(protoMsg); err != nil {
		// SIGNALLING-V2-TODO: filter out logging expected errors
		d.params.Logger.Errorw("could not send message", err)
		return err
	}

	return nil
}

func (d *dataChannelMessageSink) IsClosed() bool {
	// SIGNALLING-V2-TODO
	return false
}

func (d *dataChannelMessageSink) Close() {
	// SIGNALLING-V2-TODO
}

func (d *dataChannelMessageSink) ConnectionID() livekit.ConnectionID {
	// SIGNALLING-V2-TODO
	return ""
}
