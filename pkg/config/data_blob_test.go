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

package config

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
)

func TestLimitConfig_CanAddDataBlob(t *testing.T) {
	blob := func(key, contents string) *livekit.DataBlob {
		return &livekit.DataBlob{
			Key:      &livekit.DataBlobKey{Key: &livekit.DataBlobKey_Generic{Generic: key}},
			Contents: []byte(contents),
		}
	}

	for _, test := range []struct {
		name     string
		existing []*livekit.DataBlob
		toAdd    *livekit.DataBlob
		allowed  bool
	}{
		{
			name:     "equal size replacement",
			existing: []*livekit.DataBlob{blob("a", "1234")},
			toAdd:    blob("a", "5678"),
			allowed:  true,
		},
		{
			name:     "shrinking replacement",
			existing: []*livekit.DataBlob{blob("a", "1234")},
			toAdd:    blob("a", "56"),
			allowed:  true,
		},
		{
			name:     "growing replacement within limit",
			existing: []*livekit.DataBlob{blob("a", "12")},
			toAdd:    blob("a", "5678"),
			allowed:  true,
		},
		{
			name:     "oversized replacement",
			existing: []*livekit.DataBlob{blob("a", "1234")},
			toAdd:    blob("a", "56789"),
			allowed:  false,
		},
		{
			name:     "distinct key within limit",
			existing: []*livekit.DataBlob{blob("a", "1")},
			toAdd:    blob("b", "23"),
			allowed:  true,
		},
		{
			name:     "distinct key over limit",
			existing: []*livekit.DataBlob{blob("a", "1")},
			toAdd:    blob("b", "234"),
			allowed:  false,
		},
		{
			name:     "replacement with unrelated key within limit",
			existing: []*livekit.DataBlob{blob("a", "1"), blob("b", "23")},
			toAdd:    blob("a", "4"),
			allowed:  true,
		},
		{
			name:     "unrelated key still counts toward limit",
			existing: []*livekit.DataBlob{blob("a", "1"), blob("b", "23")},
			toAdd:    blob("a", "45"),
			allowed:  false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			limit := LimitConfig{MaxDataBlobSize: 5}
			require.Equal(t, test.allowed, limit.CanAddDataBlob(test.existing, test.toAdd))
		})
	}
}
