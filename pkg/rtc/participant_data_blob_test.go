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
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

func newTestDataBlob() *ParticipantDataBlob {
	return NewParticipantDataBlob(ParticipantDataBlobParams{
		Logger: logger.GetLogger(),
	})
}

func genericKey(name string) *livekit.DataBlobKey {
	return &livekit.DataBlobKey{
		Key: &livekit.DataBlobKey_Generic{
			Generic: name,
		},
	}
}

func TestParticipantDataBlob_AddAndGet(t *testing.T) {
	a := newTestDataBlob()

	key := genericKey("blob-1")
	contents := []byte("definition-bytes")

	a.Add(&livekit.DataBlob{Key: key, Contents: contents})

	got := a.Get(key)
	require.NotNil(t, got)
	require.Equal(t, key.String(), got.Key.String())
	require.Equal(t, contents, got.Contents)
}

func TestParticipantDataBlob_AddOverwrites(t *testing.T) {
	a := newTestDataBlob()

	key := genericKey("blob-1")
	a.Add(&livekit.DataBlob{Key: key, Contents: []byte("v1")})
	a.Add(&livekit.DataBlob{Key: key, Contents: []byte("v2")})

	got := a.Get(key)
	require.NotNil(t, got)
	require.Equal(t, []byte("v2"), got.Contents)

	require.Len(t, a.GetAll(), 1)
}

func TestParticipantDataBlob_DistinctKeys(t *testing.T) {
	a := newTestDataBlob()

	key1 := genericKey("blob-1")
	key2 := genericKey("blob-2")

	a.Add(&livekit.DataBlob{Key: key1, Contents: []byte("c1")})
	a.Add(&livekit.DataBlob{Key: key2, Contents: []byte("c2")})

	got1 := a.Get(key1)
	require.NotNil(t, got1)
	require.Equal(t, []byte("c1"), got1.Contents)

	got2 := a.Get(key2)
	require.NotNil(t, got2)
	require.Equal(t, []byte("c2"), got2.Contents)

	require.Len(t, a.GetAll(), 2)
}

func TestParticipantDataBlob_Delete(t *testing.T) {
	a := newTestDataBlob()

	key := genericKey("blob-1")
	a.Add(&livekit.DataBlob{Key: key, Contents: []byte("definition")})

	a.Delete(key)
	require.Nil(t, a.Get(key))
	require.Empty(t, a.GetAll())

	// deleting a non-existent key is a no-op
	a.Delete(key)
	require.Empty(t, a.GetAll())
}

func TestParticipantDataBlob_NilKey(t *testing.T) {
	a := newTestDataBlob()

	// nil key should be silently ignored, not panic
	a.Add(&livekit.DataBlob{Contents: []byte("definition")})
	require.Empty(t, a.GetAll())

	require.Nil(t, a.Get(nil))

	a.Delete(nil)
	require.Empty(t, a.GetAll())
}

func TestParticipantDataBlob_GetMissing(t *testing.T) {
	a := newTestDataBlob()

	require.Nil(t, a.Get(genericKey("missing")))
}

func TestParticipantDataBlob_GetAllContents(t *testing.T) {
	a := newTestDataBlob()

	key1 := genericKey("blob-1")
	key2 := genericKey("blob-2")

	a.Add(&livekit.DataBlob{Key: key1, Contents: []byte("def-1")})
	a.Add(&livekit.DataBlob{Key: key2, Contents: []byte("def-2")})

	all := a.GetAll()
	require.Len(t, all, 2)
	for _, db := range all {
		switch key := db.Key.Key.(type) {
		case *livekit.DataBlobKey_Generic:
			switch key.Generic {
			case "blob-1":
				require.Equal(t, []byte("def-1"), db.Contents)
			case "blob-2":
				require.Equal(t, []byte("def-2"), db.Contents)
			default:
				require.Fail(t, "unexpected key", key.Generic)
			}
		default:
			require.Fail(t, "unexpected key type", "Generic")
		}
	}
}

func TestParticipantDataBlob_ConcurrentAccess(t *testing.T) {
	a := newTestDataBlob()

	const numGoroutines = 16
	const opsPerGoroutine = 100

	var wg sync.WaitGroup
	wg.Add(numGoroutines)
	for g := 0; g < numGoroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < opsPerGoroutine; i++ {
				key := genericKey(fmt.Sprintf("blob-%d", g%8))
				a.Add(&livekit.DataBlob{Key: key, Contents: []byte("v")})
				_ = a.Get(key)
				_ = a.GetAll()
				if i%3 == 0 {
					a.Delete(key)
				}
			}
		}(g)
	}
	wg.Wait()
}
