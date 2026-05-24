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
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

func newTestAsyncAttributes() *ParticipantAsyncAttributes {
	return NewParticipantAsyncAttributes(ParticipantAsyncAttributesParams{
		Logger: logger.GetLogger(),
	})
}

func TestParticipantAsyncAttributes_AddAndGet(t *testing.T) {
	a := newTestAsyncAttributes()

	id := &livekit.DataTrackSchemaId{
		Name:     "schema-1",
		Encoding: livekit.DataTrackSchemaEncoding_DATA_TRACK_SCHEMA_ENCODING_PROTOBUF,
	}
	value := []byte("definition-bytes")

	a.Add(&livekit.DataTrackSchemaDefinition{Id: id, Definition: value})

	got := a.Get(id)
	require.NotNil(t, got)
	require.Equal(t, id.Name, got.Id.Name)
	require.Equal(t, id.Encoding, got.Id.Encoding)
	require.Equal(t, value, got.Definition)
}

func TestParticipantAsyncAttributes_AddOverwrites(t *testing.T) {
	a := newTestAsyncAttributes()

	id := &livekit.DataTrackSchemaId{
		Name:     "schema-1",
		Encoding: livekit.DataTrackSchemaEncoding_DATA_TRACK_SCHEMA_ENCODING_PROTOBUF,
	}
	a.Add(&livekit.DataTrackSchemaDefinition{Id: id, Definition: []byte("v1")})
	a.Add(&livekit.DataTrackSchemaDefinition{Id: id, Definition: []byte("v2")})

	got := a.Get(id)
	require.NotNil(t, got)
	require.Equal(t, []byte("v2"), got.Definition)

	require.Len(t, a.GetAll(), 1)
	allIDs := a.GetAllIDs()
	require.Len(t, allIDs, 1)
	require.Equal(t, id.Name, allIDs[0].Name)
	require.Equal(t, id.Encoding, allIDs[0].Encoding)
}

func TestParticipantAsyncAttributes_DifferentEncodingsAreDistinct(t *testing.T) {
	a := newTestAsyncAttributes()

	idProto := &livekit.DataTrackSchemaId{
		Name:     "schema-1",
		Encoding: livekit.DataTrackSchemaEncoding_DATA_TRACK_SCHEMA_ENCODING_PROTOBUF,
	}
	idJSON := &livekit.DataTrackSchemaId{
		Name:     "schema-1",
		Encoding: livekit.DataTrackSchemaEncoding_DATA_TRACK_SCHEMA_ENCODING_JSON_SCHEMA,
	}

	a.Add(&livekit.DataTrackSchemaDefinition{Id: idProto, Definition: []byte("proto-def")})
	a.Add(&livekit.DataTrackSchemaDefinition{Id: idJSON, Definition: []byte("json-def")})

	gotProto := a.Get(idProto)
	require.NotNil(t, gotProto)
	require.Equal(t, []byte("proto-def"), gotProto.Definition)

	gotJSON := a.Get(idJSON)
	require.NotNil(t, gotJSON)
	require.Equal(t, []byte("json-def"), gotJSON.Definition)

	require.Len(t, a.GetAll(), 2)
	require.Len(t, a.GetAllIDs(), 2)
}

func TestParticipantAsyncAttributes_Delete(t *testing.T) {
	a := newTestAsyncAttributes()

	id := &livekit.DataTrackSchemaId{
		Name:     "schema-1",
		Encoding: livekit.DataTrackSchemaEncoding_DATA_TRACK_SCHEMA_ENCODING_PROTOBUF,
	}
	a.Add(&livekit.DataTrackSchemaDefinition{
		Id:         id,
		Definition: []byte("definition"),
	})

	a.Delete(id)
	require.Nil(t, a.Get(id))
	require.Empty(t, a.GetAll())

	// deleting a non-existent key is a no-op
	a.Delete(id)
	require.Empty(t, a.GetAll())
}

func TestParticipantAsyncAttributes_NilId(t *testing.T) {
	a := newTestAsyncAttributes()

	// nil id should be silently ignored, not panic
	a.Add(&livekit.DataTrackSchemaDefinition{Definition: []byte("definition")})
	require.Empty(t, a.GetAll())

	require.Nil(t, a.Get(nil))

	a.Delete(nil)
	require.Empty(t, a.GetAll())
}

func TestParticipantAsyncAttributes_GetMissing(t *testing.T) {
	a := newTestAsyncAttributes()

	id := &livekit.DataTrackSchemaId{
		Name:     "missing",
		Encoding: livekit.DataTrackSchemaEncoding_DATA_TRACK_SCHEMA_ENCODING_PROTOBUF,
	}
	require.Nil(t, a.Get(id))
}

func TestParticipantAsyncAttributes_GetAllContents(t *testing.T) {
	a := newTestAsyncAttributes()

	id1 := &livekit.DataTrackSchemaId{
		Name:     "schema-1",
		Encoding: livekit.DataTrackSchemaEncoding_DATA_TRACK_SCHEMA_ENCODING_PROTOBUF,
	}
	id2 := &livekit.DataTrackSchemaId{
		Name:     "schema-2",
		Encoding: livekit.DataTrackSchemaEncoding_DATA_TRACK_SCHEMA_ENCODING_FLATBUFFER,
	}

	a.Add(&livekit.DataTrackSchemaDefinition{Id: id1, Definition: []byte("def-1")})
	a.Add(&livekit.DataTrackSchemaDefinition{Id: id2, Definition: []byte("def-2")})

	all := a.GetAll()
	require.Len(t, all, 2)
	for _, aa := range all {
		switch aa.Id.Name {
		case "schema-1":
			require.Equal(t, []byte("def-1"), aa.Definition)
		case "schema-2":
			require.Equal(t, []byte("def-2"), aa.Definition)
		default:
			require.Fail(t, "unexpected name", aa.Id.Name)
		}
	}
}

func TestParticipantAsyncAttributes_ConcurrentAccess(t *testing.T) {
	a := newTestAsyncAttributes()

	const numGoroutines = 16
	const opsPerGoroutine = 100

	var wg sync.WaitGroup
	wg.Add(numGoroutines)
	for g := 0; g < numGoroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < opsPerGoroutine; i++ {
				id := &livekit.DataTrackSchemaId{
					Name:     "schema",
					Encoding: livekit.DataTrackSchemaEncoding(g % 8),
				}
				a.Add(&livekit.DataTrackSchemaDefinition{Id: id, Definition: []byte("v")})
				_ = a.Get(id)
				_ = a.GetAll()
				if i%3 == 0 {
					a.Delete(id)
				}
			}
		}(g)
	}
	wg.Wait()
}

func TestToKeyFromKey(t *testing.T) {
	ids := []*livekit.DataTrackSchemaId{
		{
			Name:     "schema-1",
			Encoding: livekit.DataTrackSchemaEncoding_DATA_TRACK_SCHEMA_ENCODING_PROTOBUF,
		},
		{
			Name:     "another-schema",
			Encoding: livekit.DataTrackSchemaEncoding_DATA_TRACK_SCHEMA_ENCODING_JSON_SCHEMA,
		},
		{
			Name:     "x",
			Encoding: livekit.DataTrackSchemaEncoding_DATA_TRACK_SCHEMA_ENCODING_UNSPECIFIED,
		},
	}
	for _, id := range ids {
		t.Run(id.Name, func(t *testing.T) {
			key := ToParticipantAsyncAttributeKey(id)
			roundTripped := fromParticipantAsyncAttributeKey(key)
			require.Equal(t, id.Name, roundTripped.Name)
			require.Equal(t, id.Encoding, roundTripped.Encoding)
		})
	}
}
