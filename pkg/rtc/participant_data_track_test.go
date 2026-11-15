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

package rtc

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/routing/routingfakes"
	"github.com/livekit/protocol/livekit"
)

func TestUnpublishDataTrackRejectsInvalidHandle(t *testing.T) {
	for _, test := range []struct {
		name   string
		handle uint32
	}{
		{name: "handle wraps to published track", handle: 65537},
		{name: "zero handle", handle: 0},
		{name: "first out-of-range handle", handle: 65536},
		{name: "maximum uint32 handle", handle: ^uint32(0)},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := newParticipantForTest("test")
			p.params.EnableDataTracks = true
			sink := p.params.Sink.(*routingfakes.FakeMessageSink)

			p.HandlePublishDataTrackRequest(&livekit.PublishDataTrackRequest{PubHandle: 1, Name: "test-track"})
			require.NotNil(t, p.UpDataTrackManager.GetPublishedDataTrack(1))
			require.Equal(t, 1, sink.WriteMessageCallCount())

			p.HandleUnpublishDataTrackRequest(&livekit.UnpublishDataTrackRequest{PubHandle: test.handle})

			require.Equal(t, 2, sink.WriteMessageCallCount())
			response := sink.WriteMessageArgsForCall(1).(*livekit.SignalResponse).GetRequestResponse()
			if assert.NotNil(t, response, "expected INVALID_HANDLE request response for handle %d", test.handle) {
				assert.Equal(t, livekit.RequestResponse_INVALID_HANDLE, response.Reason)
			}
			assert.NotNil(t, p.UpDataTrackManager.GetPublishedDataTrack(1), "published track must remain after handle %d", test.handle)
		})
	}
}

func TestPublishDataTrackRejectsInvalidHandle(t *testing.T) {
	for _, handle := range []uint32{0, 65536, 65537, ^uint32(0)} {
		t.Run(fmt.Sprint(handle), func(t *testing.T) {
			p := newParticipantForTest("test")
			p.params.EnableDataTracks = true
			sink := p.params.Sink.(*routingfakes.FakeMessageSink)

			p.HandlePublishDataTrackRequest(&livekit.PublishDataTrackRequest{PubHandle: handle, Name: "test-track"})

			require.Equal(t, 1, sink.WriteMessageCallCount())
			response := lastRequestResponse(t, sink, 0)
			require.Equal(t, livekit.RequestResponse_INVALID_HANDLE, response.Reason)
			require.Empty(t, p.UpDataTrackManager.GetPublishedDataTracks())
		})
	}
}

func TestDataTrackValidHandles(t *testing.T) {
	for _, handle := range []uint32{1, 32768, 65535} {
		t.Run(fmt.Sprint(handle), func(t *testing.T) {
			p := newParticipantForTest("test")
			p.params.EnableDataTracks = true
			sink := p.params.Sink.(*routingfakes.FakeMessageSink)

			p.HandlePublishDataTrackRequest(&livekit.PublishDataTrackRequest{PubHandle: handle, Name: "test-track"})

			require.NotNil(t, p.UpDataTrackManager.GetPublishedDataTrack(uint16(handle)))
			require.Equal(t, 1, sink.WriteMessageCallCount())
			require.NotNil(t, sink.WriteMessageArgsForCall(0).(*livekit.SignalResponse).GetPublishDataTrackResponse())

			p.HandleUnpublishDataTrackRequest(&livekit.UnpublishDataTrackRequest{PubHandle: handle})

			require.Empty(t, p.UpDataTrackManager.GetPublishedDataTracks())
			require.Equal(t, 2, sink.WriteMessageCallCount())
			require.NotNil(t, sink.WriteMessageArgsForCall(1).(*livekit.SignalResponse).GetUnpublishDataTrackResponse())
		})
	}
}
