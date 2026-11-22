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

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/rpc"
	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/livekit-server/pkg/service/servicefakes"
	"github.com/livekit/livekit-server/pkg/telemetry/telemetryfakes"
)

// A concurrent DeleteIngress can remove the ingress state key between the two
// non-atomic reads LoadIngress performs, making it return an IngressInfo with a
// nil State and no error. UpdateIngressState used to panic dereferencing it,
// bringing down the whole node.
func TestUpdateIngressStateWithNilState(t *testing.T) {
	is := &servicefakes.FakeIngressStore{}
	is.LoadIngressReturns(&livekit.IngressInfo{IngressId: "IN_test"}, nil)

	io, err := service.NewIOInfoService(nil, nil, is, nil, &telemetryfakes.FakeTelemetryService{})
	require.NoError(t, err)

	_, err = io.UpdateIngressState(context.Background(), &rpc.UpdateIngressStateRequest{
		IngressId: "IN_test",
		State:     &livekit.IngressState{Status: livekit.IngressState_ENDPOINT_INACTIVE},
	})
	require.NoError(t, err)
}
