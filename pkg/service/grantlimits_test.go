// Copyright 2024 LiveKit, Inc.
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

package service

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/protocol/auth"
)

// grantLimitCase is an over-limit token that every join path must reject.
type grantLimitCase struct {
	name  string
	limit config.LimitConfig
	grant *auth.ClaimGrants
	err   error
}

// grantLimitCases returns the same over-limit tokens for each join path's test.
// ValidateConnectRequest is covered from the service_test package; see utils_test.go.
func grantLimitCases() []grantLimitCase {
	return []grantLimitCase{
		{
			name:  "name exceeds",
			limit: config.LimitConfig{MaxParticipantNameLength: 5},
			grant: &auth.ClaimGrants{Identity: "id", Name: "toolongname", Video: &auth.VideoGrant{RoomJoin: true, Room: "room"}},
			err:   ErrNameExceedsLimits,
		},
		{
			name:  "metadata exceeds",
			limit: config.LimitConfig{MaxMetadataSize: 5},
			grant: &auth.ClaimGrants{Identity: "id", Metadata: "abcdefg", Video: &auth.VideoGrant{RoomJoin: true, Room: "room"}},
			err:   ErrMetadataExceedsLimits,
		},
		{
			name:  "attributes exceeds",
			limit: config.LimitConfig{MaxAttributesSize: 5},
			grant: &auth.ClaimGrants{Identity: "id", Attributes: map[string]string{"k": "abcdefg"}, Video: &auth.VideoGrant{RoomJoin: true, Room: "room"}},
			err:   ErrAttributeExceedsLimits,
		},
	}
}

func grantRequest(grant *auth.ClaimGrants) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	return r.WithContext(WithGrants(r.Context(), grant, "apikey"))
}

func TestWHIPValidateCreateGrantLimits(t *testing.T) {
	for _, tc := range grantLimitCases() {
		t.Run(tc.name, func(t *testing.T) {
			svc := &WHIPService{config: &config.Config{Limit: tc.limit}}
			_, code, err := svc.validateCreate(httptest.NewRecorder(), grantRequest(tc.grant))
			require.ErrorIs(t, err, tc.err)
			require.Equal(t, http.StatusBadRequest, code)
		})
	}
}
