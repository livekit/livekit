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

func newParticipantWithAsyncAttributes(t *testing.T, enabled bool, maxSize uint32) *ParticipantImpl {
	t.Helper()
	p := newParticipantForTest("test")
	p.params.EnableParticipantAsyncAttributes = enabled
	p.params.LimitConfig = config.LimitConfig{
		MaxAsyncAttributesSize: maxSize,
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

func TestHandleDefineDataTrackSchemaRequest(t *testing.T) {
	t.Run("returns NOT_ALLOWED when feature not enabled", func(t *testing.T) {
		p := newParticipantWithAsyncAttributes(t, false, 0)
		sink := p.params.Sink.(*routingfakes.FakeMessageSink)

		req := &livekit.DefineDataTrackSchemaRequest{
			SchemaDefinition: &livekit.DataTrackSchemaDefinition{
				Id: &livekit.DataTrackSchemaId{
					Name:     "schema-1",
					Encoding: livekit.DataTrackSchemaEncoding_DATA_TRACK_SCHEMA_ENCODING_PROTOBUF,
				},
				Definition: []byte("def"),
			},
		}
		p.HandleDefineDataTrackSchemaRequest(req)

		require.Equal(t, 1, sink.WriteMessageCallCount())
		rr := lastRequestResponse(t, sink, 0)
		require.Equal(t, livekit.RequestResponse_NOT_ALLOWED, rr.Reason)
		require.NotNil(t, rr.GetDefineDataTrackSchema())
		require.Empty(t, p.asyncAttributes.GetAll())
	})

	t.Run("returns INVALID_REQUEST when schema definition is nil", func(t *testing.T) {
		p := newParticipantWithAsyncAttributes(t, true, 0)
		sink := p.params.Sink.(*routingfakes.FakeMessageSink)

		p.HandleDefineDataTrackSchemaRequest(&livekit.DefineDataTrackSchemaRequest{})

		require.Equal(t, 1, sink.WriteMessageCallCount())
		rr := lastRequestResponse(t, sink, 0)
		require.Equal(t, livekit.RequestResponse_INVALID_REQUEST, rr.Reason)
		require.Empty(t, p.asyncAttributes.GetAll())
	})

	t.Run("returns INVALID_REQUEST when id is nil", func(t *testing.T) {
		p := newParticipantWithAsyncAttributes(t, true, 0)
		sink := p.params.Sink.(*routingfakes.FakeMessageSink)

		p.HandleDefineDataTrackSchemaRequest(&livekit.DefineDataTrackSchemaRequest{
			SchemaDefinition: &livekit.DataTrackSchemaDefinition{
				Definition: []byte("def"),
			},
		})

		require.Equal(t, 1, sink.WriteMessageCallCount())
		rr := lastRequestResponse(t, sink, 0)
		require.Equal(t, livekit.RequestResponse_INVALID_REQUEST, rr.Reason)
	})

	t.Run("returns INVALID_REQUEST when name is empty", func(t *testing.T) {
		p := newParticipantWithAsyncAttributes(t, true, 0)
		sink := p.params.Sink.(*routingfakes.FakeMessageSink)

		p.HandleDefineDataTrackSchemaRequest(&livekit.DefineDataTrackSchemaRequest{
			SchemaDefinition: &livekit.DataTrackSchemaDefinition{
				Id: &livekit.DataTrackSchemaId{
					Name:     "",
					Encoding: livekit.DataTrackSchemaEncoding_DATA_TRACK_SCHEMA_ENCODING_PROTOBUF,
				},
				Definition: []byte("def"),
			},
		})

		require.Equal(t, 1, sink.WriteMessageCallCount())
		rr := lastRequestResponse(t, sink, 0)
		require.Equal(t, livekit.RequestResponse_INVALID_REQUEST, rr.Reason)
	})

	t.Run("returns INVALID_REQUEST when name exceeds 256 chars", func(t *testing.T) {
		p := newParticipantWithAsyncAttributes(t, true, 0)
		sink := p.params.Sink.(*routingfakes.FakeMessageSink)

		p.HandleDefineDataTrackSchemaRequest(&livekit.DefineDataTrackSchemaRequest{
			SchemaDefinition: &livekit.DataTrackSchemaDefinition{
				Id: &livekit.DataTrackSchemaId{
					Name:     strings.Repeat("a", 257),
					Encoding: livekit.DataTrackSchemaEncoding_DATA_TRACK_SCHEMA_ENCODING_PROTOBUF,
				},
				Definition: []byte("def"),
			},
		})

		require.Equal(t, 1, sink.WriteMessageCallCount())
		rr := lastRequestResponse(t, sink, 0)
		require.Equal(t, livekit.RequestResponse_INVALID_REQUEST, rr.Reason)
	})

	t.Run("returns INVALID_REQUEST when definition is empty", func(t *testing.T) {
		p := newParticipantWithAsyncAttributes(t, true, 0)
		sink := p.params.Sink.(*routingfakes.FakeMessageSink)

		p.HandleDefineDataTrackSchemaRequest(&livekit.DefineDataTrackSchemaRequest{
			SchemaDefinition: &livekit.DataTrackSchemaDefinition{
				Id: &livekit.DataTrackSchemaId{
					Name:     "schema-1",
					Encoding: livekit.DataTrackSchemaEncoding_DATA_TRACK_SCHEMA_ENCODING_PROTOBUF,
				},
				Definition: nil,
			},
		})

		require.Equal(t, 1, sink.WriteMessageCallCount())
		rr := lastRequestResponse(t, sink, 0)
		require.Equal(t, livekit.RequestResponse_INVALID_REQUEST, rr.Reason)
		require.Empty(t, p.asyncAttributes.GetAll())
	})

	t.Run("returns LIMIT_EXCEEDED when adding would breach the limit", func(t *testing.T) {
		p := newParticipantWithAsyncAttributes(t, true, 16)
		sink := p.params.Sink.(*routingfakes.FakeMessageSink)

		p.HandleDefineDataTrackSchemaRequest(&livekit.DefineDataTrackSchemaRequest{
			SchemaDefinition: &livekit.DataTrackSchemaDefinition{
				Id: &livekit.DataTrackSchemaId{
					Name:     "schema-1",
					Encoding: livekit.DataTrackSchemaEncoding_DATA_TRACK_SCHEMA_ENCODING_PROTOBUF,
				},
				Definition: []byte(strings.Repeat("x", 32)),
			},
		})

		require.Equal(t, 1, sink.WriteMessageCallCount())
		rr := lastRequestResponse(t, sink, 0)
		require.Equal(t, livekit.RequestResponse_LIMIT_EXCEEDED, rr.Reason)
		require.Empty(t, p.asyncAttributes.GetAll())
	})

	t.Run("stores a valid definition and sends no response", func(t *testing.T) {
		p := newParticipantWithAsyncAttributes(t, true, 0)
		sink := p.params.Sink.(*routingfakes.FakeMessageSink)

		id := &livekit.DataTrackSchemaId{
			Name:     "schema-1",
			Encoding: livekit.DataTrackSchemaEncoding_DATA_TRACK_SCHEMA_ENCODING_PROTOBUF,
		}
		def := []byte("definition-bytes")

		p.HandleDefineDataTrackSchemaRequest(&livekit.DefineDataTrackSchemaRequest{
			SchemaDefinition: &livekit.DataTrackSchemaDefinition{
				Id:         id,
				Definition: def,
			},
		})

		// on success no response is sent to the client
		require.Equal(t, 0, sink.WriteMessageCallCount())

		stored := p.asyncAttributes.Get(id)
		require.NotNil(t, stored)
		require.Equal(t, def, stored.Definition)
	})
}

func TestHandleGetDataTrackSchemaRequest(t *testing.T) {
	t.Run("returns INVALID_REQUEST when schema id is missing", func(t *testing.T) {
		p := newParticipantWithAsyncAttributes(t, true, 0)
		sink := p.params.Sink.(*routingfakes.FakeMessageSink)

		p.HandleGetDataTrackSchemaRequest(&livekit.GetDataTrackSchemaRequest{
			ParticipantIdentity: "other",
		})

		require.Equal(t, 1, sink.WriteMessageCallCount())
		rr := lastRequestResponse(t, sink, 0)
		require.Equal(t, livekit.RequestResponse_INVALID_REQUEST, rr.Reason)
	})

	t.Run("forwards request to listener when schema id is provided", func(t *testing.T) {
		p := newParticipantWithAsyncAttributes(t, true, 0)
		listener := p.params.ParticipantListener.(*typesfakes.FakeLocalParticipantListener)

		req := &livekit.GetDataTrackSchemaRequest{
			ParticipantIdentity: "other",
			SchemaId: &livekit.DataTrackSchemaId{
				Name:     "schema-1",
				Encoding: livekit.DataTrackSchemaEncoding_DATA_TRACK_SCHEMA_ENCODING_PROTOBUF,
			},
		}
		p.HandleGetDataTrackSchemaRequest(req)

		require.Equal(t, 1, listener.OnGetDataTrackSchemaCallCount())
		gotParticipant, gotReq := listener.OnGetDataTrackSchemaArgsForCall(0)
		require.Equal(t, p, gotParticipant)
		require.Equal(t, req, gotReq)
	})
}

func TestGetDataTrackSchema(t *testing.T) {
	p := newParticipantWithAsyncAttributes(t, true, 0)

	id := &livekit.DataTrackSchemaId{
		Name:     "schema-1",
		Encoding: livekit.DataTrackSchemaEncoding_DATA_TRACK_SCHEMA_ENCODING_PROTOBUF,
	}
	require.Nil(t, p.GetDataTrackSchema(id))

	p.asyncAttributes.Add(id, []byte("definition"))
	got := p.GetDataTrackSchema(id)
	require.NotNil(t, got)
	require.Equal(t, id.Name, got.Id.Name)
	require.Equal(t, []byte("definition"), got.Definition)
}

func TestProcessGetDataTrackSchemaRequest(t *testing.T) {
	t.Run("returns NOT_FOUND when publisher is nil", func(t *testing.T) {
		p := newParticipantWithAsyncAttributes(t, true, 0)
		sink := p.params.Sink.(*routingfakes.FakeMessageSink)

		p.ProcessGetDataTrackSchemaRequest(&livekit.GetDataTrackSchemaRequest{
			SchemaId: &livekit.DataTrackSchemaId{
				Name:     "schema-1",
				Encoding: livekit.DataTrackSchemaEncoding_DATA_TRACK_SCHEMA_ENCODING_PROTOBUF,
			},
		}, nil)

		require.Equal(t, 1, sink.WriteMessageCallCount())
		rr := lastRequestResponse(t, sink, 0)
		require.Equal(t, livekit.RequestResponse_NOT_FOUND, rr.Reason)
		require.Contains(t, rr.Message, "participant")
	})

	t.Run("returns NOT_FOUND when publisher has no matching schema", func(t *testing.T) {
		p := newParticipantWithAsyncAttributes(t, true, 0)
		sink := p.params.Sink.(*routingfakes.FakeMessageSink)

		publisher := &typesfakes.FakeParticipant{}
		publisher.GetDataTrackSchemaReturns(nil)

		req := &livekit.GetDataTrackSchemaRequest{
			SchemaId: &livekit.DataTrackSchemaId{
				Name:     "schema-1",
				Encoding: livekit.DataTrackSchemaEncoding_DATA_TRACK_SCHEMA_ENCODING_PROTOBUF,
			},
		}
		p.ProcessGetDataTrackSchemaRequest(req, publisher)

		require.Equal(t, 1, publisher.GetDataTrackSchemaCallCount())
		require.Equal(t, req.SchemaId, publisher.GetDataTrackSchemaArgsForCall(0))

		require.Equal(t, 1, sink.WriteMessageCallCount())
		rr := lastRequestResponse(t, sink, 0)
		require.Equal(t, livekit.RequestResponse_NOT_FOUND, rr.Reason)
	})

	t.Run("sends schema response when publisher has a matching schema", func(t *testing.T) {
		p := newParticipantWithAsyncAttributes(t, true, 0)
		sink := p.params.Sink.(*routingfakes.FakeMessageSink)

		id := &livekit.DataTrackSchemaId{
			Name:     "schema-1",
			Encoding: livekit.DataTrackSchemaEncoding_DATA_TRACK_SCHEMA_ENCODING_PROTOBUF,
		}
		def := &livekit.DataTrackSchemaDefinition{
			Id:         id,
			Definition: []byte("definition-bytes"),
		}

		publisher := &typesfakes.FakeParticipant{}
		publisher.GetDataTrackSchemaReturns(def)

		p.ProcessGetDataTrackSchemaRequest(&livekit.GetDataTrackSchemaRequest{
			SchemaId: id,
		}, publisher)

		require.Equal(t, 1, sink.WriteMessageCallCount())
		msg := sink.WriteMessageArgsForCall(0).(*livekit.SignalResponse)
		response, ok := msg.Message.(*livekit.SignalResponse_GetDataTrackSchemaResponse)
		require.True(t, ok, "expected SignalResponse_GetDataTrackSchemaResponse, got %T", msg.Message)
		require.Equal(t, def, response.GetDataTrackSchemaResponse.SchemaDefinition)
	})
}
