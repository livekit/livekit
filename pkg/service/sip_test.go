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

package service_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/psrpc"
)

func TestCreateSIPParticipantSourceCallUnsupported(t *testing.T) {
	_, err := (&service.SIPService{}).CreateSIPParticipant(context.Background(), &livekit.CreateSIPParticipantRequest{
		SipSourceCallId: "SCL_1",
	})
	code, _ := psrpc.GetErrorCode(err)
	require.Equal(t, psrpc.Unimplemented, code)
}
