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
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing/routingfakes"
	"github.com/livekit/livekit-server/pkg/rtc/types/typesfakes"
)

func newParticipantWithDataBlob(t *testing.T, enabled bool, maxKeyLength int, maxSize uint32) *ParticipantImpl {
	t.Helper()
	p := newParticipantForTest("test")
	p.params.EnableParticipantDataBlob = enabled
	p.params.LimitConfig = config.LimitConfig{
		MaxDataBlobKeyLength: maxKeyLength,
		MaxDataBlobSize:      maxSize,
	}
	return p
}

func lastRequestResponse(t *testing.T, sink *routingfakes.FakeMessageSink, idx int) *livekit.RequestResponse {
	t.Helper()
	msg := sink.WriteMessageArgsForCall(idx).(*livekit.SignalResponse)
	rr, ok := msg.Message.(*livekit.SignalResponse_RequestResponse)
	require.True(t, ok, "expected SignalResponse_RequestResponse, got %T", msg.Message)
	return rr.RequestResponse
}

func TestHandleStoreDataBlobRequest(t *testing.T) {
	t.Run("returns NOT_ALLOWED when feature not enabled", func(t *testing.T) {
		p := newParticipantWithDataBlob(t, false, 0, 0)
		sink := p.params.Sink.(*routingfakes.FakeMessageSink)

		req := &livekit.StoreDataBlobRequest{
			Blob: &livekit.DataBlob{
				Key:      genericKey("blob-1"),
				Contents: []byte("def"),
			},
		}
		p.HandleStoreDataBlobRequest(req)

		require.Equal(t, 1, sink.WriteMessageCallCount())
		rr := lastRequestResponse(t, sink, 0)
		require.Equal(t, livekit.RequestResponse_NOT_ALLOWED, rr.Reason)
		require.Empty(t, p.dataBlob.GetAll())
	})

	t.Run("returns INVALID_REQUEST when blob is nil", func(t *testing.T) {
		p := newParticipantWithDataBlob(t, true, 0, 0)
		sink := p.params.Sink.(*routingfakes.FakeMessageSink)

		p.HandleStoreDataBlobRequest(&livekit.StoreDataBlobRequest{})

		require.Equal(t, 1, sink.WriteMessageCallCount())
		rr := lastRequestResponse(t, sink, 0)
		require.Equal(t, livekit.RequestResponse_INVALID_REQUEST, rr.Reason)
		require.Empty(t, p.dataBlob.GetAll())
	})

	t.Run("returns INVALID_REQUEST when key is nil", func(t *testing.T) {
		p := newParticipantWithDataBlob(t, true, 0, 0)
		sink := p.params.Sink.(*routingfakes.FakeMessageSink)

		p.HandleStoreDataBlobRequest(&livekit.StoreDataBlobRequest{
			Blob: &livekit.DataBlob{
				Contents: []byte("def"),
			},
		})

		require.Equal(t, 1, sink.WriteMessageCallCount())
		rr := lastRequestResponse(t, sink, 0)
		require.Equal(t, livekit.RequestResponse_INVALID_REQUEST, rr.Reason)
	})

	t.Run("returns INVALID_REQUEST when key has no oneof set", func(t *testing.T) {
		p := newParticipantWithDataBlob(t, true, 0, 0)
		sink := p.params.Sink.(*routingfakes.FakeMessageSink)

		p.HandleStoreDataBlobRequest(&livekit.StoreDataBlobRequest{
			Blob: &livekit.DataBlob{
				Key:      &livekit.DataBlobKey{},
				Contents: []byte("def"),
			},
		})

		require.Equal(t, 1, sink.WriteMessageCallCount())
		rr := lastRequestResponse(t, sink, 0)
		require.Equal(t, livekit.RequestResponse_INVALID_REQUEST, rr.Reason)
	})

	t.Run("returns INVALID_REQUEST when key exceeds length limit", func(t *testing.T) {
		p := newParticipantWithDataBlob(t, true, 5, 0)
		sink := p.params.Sink.(*routingfakes.FakeMessageSink)

		p.HandleStoreDataBlobRequest(&livekit.StoreDataBlobRequest{
			Blob: &livekit.DataBlob{
				Key:      genericKey(strings.Repeat("a", 64)),
				Contents: []byte("def"),
			},
		})

		require.Equal(t, 1, sink.WriteMessageCallCount())
		rr := lastRequestResponse(t, sink, 0)
		require.Equal(t, livekit.RequestResponse_INVALID_REQUEST, rr.Reason)
	})

	t.Run("returns INVALID_REQUEST when contents is empty", func(t *testing.T) {
		p := newParticipantWithDataBlob(t, true, 0, 0)
		sink := p.params.Sink.(*routingfakes.FakeMessageSink)

		p.HandleStoreDataBlobRequest(&livekit.StoreDataBlobRequest{
			Blob: &livekit.DataBlob{
				Key: genericKey("blob-1"),
			},
		})

		require.Equal(t, 1, sink.WriteMessageCallCount())
		rr := lastRequestResponse(t, sink, 0)
		require.Equal(t, livekit.RequestResponse_INVALID_REQUEST, rr.Reason)
		require.Empty(t, p.dataBlob.GetAll())
	})

	t.Run("returns LIMIT_EXCEEDED when adding would breach the limit", func(t *testing.T) {
		p := newParticipantWithDataBlob(t, true, 0, 16)
		sink := p.params.Sink.(*routingfakes.FakeMessageSink)

		p.HandleStoreDataBlobRequest(&livekit.StoreDataBlobRequest{
			Blob: &livekit.DataBlob{
				Key:      genericKey("blob-1"),
				Contents: []byte(strings.Repeat("x", 32)),
			},
		})

		require.Equal(t, 1, sink.WriteMessageCallCount())
		rr := lastRequestResponse(t, sink, 0)
		require.Equal(t, livekit.RequestResponse_LIMIT_EXCEEDED, rr.Reason)
		require.Empty(t, p.dataBlob.GetAll())
	})

	t.Run("stores a valid blob, notifies listener, and sends response", func(t *testing.T) {
		p := newParticipantWithDataBlob(t, true, 0, 0)
		sink := p.params.Sink.(*routingfakes.FakeMessageSink)
		listener := p.params.ParticipantListener.(*typesfakes.FakeLocalParticipantListener)

		key := genericKey("blob-1")
		contents := []byte("definition-bytes")
		blob := &livekit.DataBlob{Key: key, Contents: contents}

		p.HandleStoreDataBlobRequest(&livekit.StoreDataBlobRequest{
			RequestId: 42,
			Blob:      blob,
		})

		require.Equal(t, 1, sink.WriteMessageCallCount())
		msg := sink.WriteMessageArgsForCall(0).(*livekit.SignalResponse)
		response, ok := msg.Message.(*livekit.SignalResponse_StoreDataBlobResponse)
		require.True(t, ok, "expected SignalResponse_StoreDataBlobResponse, got %T", msg.Message)
		require.Equal(t, uint32(42), response.StoreDataBlobResponse.RequestId)
		require.Equal(t, key, response.StoreDataBlobResponse.Key)

		stored := p.dataBlob.Get(key)
		require.NotNil(t, stored)
		require.Equal(t, contents, stored.Contents)

		require.Equal(t, 1, listener.OnStoreDataBlobCallCount())
		gotParticipant, gotBlob := listener.OnStoreDataBlobArgsForCall(0)
		require.Equal(t, p, gotParticipant)
		require.Equal(t, blob, gotBlob)
	})
}

func TestHandleGetDataBlobRequest(t *testing.T) {
	t.Run("returns INVALID_REQUEST when key is missing", func(t *testing.T) {
		p := newParticipantWithDataBlob(t, true, 0, 0)
		sink := p.params.Sink.(*routingfakes.FakeMessageSink)

		p.HandleGetDataBlobRequest(&livekit.GetDataBlobRequest{
			ParticipantIdentity: "other",
		})

		require.Equal(t, 1, sink.WriteMessageCallCount())
		rr := lastRequestResponse(t, sink, 0)
		require.Equal(t, livekit.RequestResponse_INVALID_REQUEST, rr.Reason)
	})

	t.Run("forwards request to listener when key is provided", func(t *testing.T) {
		p := newParticipantWithDataBlob(t, true, 0, 0)
		listener := p.params.ParticipantListener.(*typesfakes.FakeLocalParticipantListener)

		req := &livekit.GetDataBlobRequest{
			ParticipantIdentity: "other",
			Key:                 genericKey("blob-1"),
		}
		p.HandleGetDataBlobRequest(req)

		require.Equal(t, 1, listener.OnGetDataBlobCallCount())
		gotParticipant, gotReq := listener.OnGetDataBlobArgsForCall(0)
		require.Equal(t, p, gotParticipant)
		require.Equal(t, req, gotReq)
	})
}

func TestGetDataBlob(t *testing.T) {
	p := newParticipantWithDataBlob(t, true, 0, 0)

	key := genericKey("blob-1")
	require.Nil(t, p.GetDataBlob(key))

	blob := &livekit.DataBlob{
		Key:      key,
		Contents: []byte("definition"),
	}
	p.dataBlob.Add(blob)
	got := p.GetDataBlob(key)
	require.NotNil(t, got)
	require.Equal(t, key.String(), got.Key.String())
	require.Equal(t, []byte("definition"), got.Contents)
}

func TestProcessGetDataBlobRequest(t *testing.T) {
	t.Run("returns NOT_FOUND when publisher is nil", func(t *testing.T) {
		p := newParticipantWithDataBlob(t, true, 0, 0)
		sink := p.params.Sink.(*routingfakes.FakeMessageSink)

		p.ProcessGetDataBlobRequest(&livekit.GetDataBlobRequest{
			Key: genericKey("blob-1"),
		}, nil)

		require.Equal(t, 1, sink.WriteMessageCallCount())
		rr := lastRequestResponse(t, sink, 0)
		require.Equal(t, livekit.RequestResponse_NOT_FOUND, rr.Reason)
		require.Contains(t, rr.Message, "participant")
	})

	t.Run("returns NOT_FOUND when publisher has no matching blob", func(t *testing.T) {
		p := newParticipantWithDataBlob(t, true, 0, 0)
		sink := p.params.Sink.(*routingfakes.FakeMessageSink)

		publisher := &typesfakes.FakeParticipant{}
		publisher.GetDataBlobReturns(nil)

		req := &livekit.GetDataBlobRequest{
			Key: genericKey("blob-1"),
		}
		p.ProcessGetDataBlobRequest(req, publisher)

		require.Equal(t, 1, publisher.GetDataBlobCallCount())
		require.Equal(t, req.Key, publisher.GetDataBlobArgsForCall(0))

		require.Equal(t, 1, sink.WriteMessageCallCount())
		rr := lastRequestResponse(t, sink, 0)
		require.Equal(t, livekit.RequestResponse_NOT_FOUND, rr.Reason)
	})

	t.Run("sends blob response when publisher has a matching blob", func(t *testing.T) {
		p := newParticipantWithDataBlob(t, true, 0, 0)
		sink := p.params.Sink.(*routingfakes.FakeMessageSink)

		key := genericKey("blob-1")
		blob := &livekit.DataBlob{
			Key:      key,
			Contents: []byte("definition-bytes"),
		}

		publisher := &typesfakes.FakeParticipant{}
		publisher.GetDataBlobReturns(blob)

		p.ProcessGetDataBlobRequest(&livekit.GetDataBlobRequest{
			RequestId: 42,
			Key:       key,
		}, publisher)

		require.Equal(t, 1, sink.WriteMessageCallCount())
		msg := sink.WriteMessageArgsForCall(0).(*livekit.SignalResponse)
		response, ok := msg.Message.(*livekit.SignalResponse_GetDataBlobResponse)
		require.True(t, ok, "expected SignalResponse_GetDataBlobResponse, got %T", msg.Message)
		require.Equal(t, uint32(42), response.GetDataBlobResponse.RequestId)
		require.Equal(t, blob, response.GetDataBlobResponse.Blob)
	})
}
