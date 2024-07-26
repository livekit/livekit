// Copyright 2023 LiveKit, Inc.
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
	"context"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/rpc"
)

type AgentDispatchService struct {
	agentDispatchClient rpc.TypedAgentDispatchInternalClient
}

func NewAgentDispatchService(agentDispatchClient rpc.TypedAgentDispatchInternalClient) *AgentDispatchService {
	return &AgentDispatchService{
		agentDispatchClient: agentDispatchClient,
	}
}

func (ag *AgentDispatchService) ListDispatch(ctx context.Context, req *livekit.ListAgentDispatchRequesst) (*livekit.ListAgentDispatchResponse, error) {
	err := EnsureAdminPermission(ctx, livekit.RoomName(req.Room))
	if err != nil {
		return nil, twirpAuthError(err)
	}

	return ag.agentDispatchClient.ListDispatch(ctx, rpc.RoomTopic(req.Room), req)
}

func (ag *AgentDispatchService) DeleteDispatch(ctx context.Context, req *livekit.DeleteAgentDispatchRequest) (*livekit.AgentDispatch, error) {
	return nil, nil
}

func (ag *AgentDispatchService) CreateDispatch(ctx context.Context, req *livekit.CreateAgentDispatchRequest) (*livekit.AgentDispatch, error) {
	return nil, nil
}
