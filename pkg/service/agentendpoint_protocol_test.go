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

package service

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/agent"
	"github.com/livekit/livekit-server/pkg/agent/endpoint/wire"
	"github.com/livekit/protocol/livekit"
)

func TestEndpointSettingsNegotiation(t *testing.T) {
	req := func(protocol uint32) *livekit.RegisterWorkerRequest {
		return &livekit.RegisterWorkerRequest{
			AgentName:        "test-agent",
			Deployment:       "production",
			InstanceId:       "AEI_test",
			Endpoints:        []*livekit.AgentHttp_AgentEndpoint{{Path: "/json", Methods: []string{"GET"}}},
			EndpointProtocol: protocol,
		}
	}
	h := &AgentHandler{}
	negotiate := func(h *AgentHandler, r *livekit.RegisterWorkerRequest) (*livekit.AgentHttp_AgentEndpointSettings, error) {
		res := &livekit.RegisterWorkerResponse{}
		reg := &agent.WorkerRegistration{}
		if err := h.endpointRegisterHandler(r, res, reg); err != nil {
			return nil, err
		}
		require.Equal(t, res.GetEndpointSettings(), reg.EndpointSettings,
			"the registration must carry what the worker was told")
		return res.GetEndpointSettings(), nil
	}

	// a worker predating the data plane declares endpoints with protocol 0: it
	// cannot frame at all, and the error must name that rather than read as a
	// version out of range
	_, err := negotiate(h, req(0))
	require.ErrorContains(t, err, "no endpoint protocol")

	settings, err := negotiate(h, req(wire.CurrentProtocol))
	require.NoError(t, err)
	require.Equal(t, wire.CurrentProtocol, settings.GetProtocol())

	// a worker speaking a newer version is negotiated down to what this server
	// serves; the worker frames to the version returned here
	settings, err = negotiate(h, req(wire.CurrentProtocol+42))
	require.NoError(t, err)
	require.Equal(t, wire.CurrentProtocol, settings.GetProtocol())

	noInstance := req(wire.CurrentProtocol)
	noInstance.InstanceId = ""
	_, err = negotiate(h, noInstance)
	require.ErrorContains(t, err, "instance_id")

	disabled := &AgentHandler{endpointsConfig: agent.EndpointsConfig{Disabled: true}}
	_, err = negotiate(disabled, req(wire.CurrentProtocol))
	require.ErrorContains(t, err, "disabled")

	// a registration declaring no endpoints is left alone, even with endpoints off
	bare := req(wire.CurrentProtocol)
	bare.Endpoints = nil
	settings, err = negotiate(disabled, bare)
	require.NoError(t, err)
	require.Nil(t, settings)
}
