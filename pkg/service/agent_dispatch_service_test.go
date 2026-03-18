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

package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/twitchtv/twirp"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/rpc/rpcfakes"

	"github.com/livekit/livekit-server/pkg/routing/routingfakes"
	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/livekit-server/pkg/service/servicefakes"
)

type testAgentDispatchService struct {
	*service.AgentDispatchService
	agentClient *rpcfakes.FakeTypedAgentDispatchInternalClient
	allocator   *servicefakes.FakeRoomAllocator
	router      *routingfakes.FakeRouter
}

func newTestAgentDispatchService() *testAgentDispatchService {
	agentClient := &rpcfakes.FakeTypedAgentDispatchInternalClient{}
	allocator := &servicefakes.FakeRoomAllocator{}
	router := &routingfakes.FakeRouter{}

	svc := service.NewAgentDispatchService(
		agentClient,
		rpc.NewTopicFormatter(),
		allocator,
		router,
	)

	return &testAgentDispatchService{
		AgentDispatchService: svc,
		agentClient:          agentClient,
		allocator:            allocator,
		router:               router,
	}
}

func adminContext(room string) context.Context {
	grant := &auth.ClaimGrants{
		Video: &auth.VideoGrant{RoomAdmin: true, Room: room},
	}
	return service.WithGrants(context.Background(), grant, "")
}

func nonAdminContext() context.Context {
	grant := &auth.ClaimGrants{
		Video: &auth.VideoGrant{},
	}
	return service.WithGrants(context.Background(), grant, "")
}

// --- CreateDispatch ---
func TestCreateDispatch_NilRequest(t *testing.T) {
	svc := newTestAgentDispatchService()
	_, err := svc.CreateDispatch(adminContext("testroom"), nil)
	requireTwirpCode(t, err, twirp.InvalidArgument)
}

func TestCreateDispatch_MissingRoom(t *testing.T) {
	svc := newTestAgentDispatchService()
	_, err := svc.CreateDispatch(adminContext("testroom"), &livekit.CreateAgentDispatchRequest{
		AgentName: "test-agent",
	})
	requireTwirpCode(t, err, twirp.InvalidArgument)
}

func TestCreateDispatch_MissingAgentName(t *testing.T) {
	svc := newTestAgentDispatchService()
	_, err := svc.CreateDispatch(adminContext("testroom"), &livekit.CreateAgentDispatchRequest{
		Room: "testroom",
	})
	requireTwirpCode(t, err, twirp.InvalidArgument)
}

func TestCreateDispatch_NoPermission(t *testing.T) {
	svc := newTestAgentDispatchService()
	_, err := svc.CreateDispatch(nonAdminContext(), &livekit.CreateAgentDispatchRequest{
		Room:      "testroom",
		AgentName: "test-agent",
	})
	requireTwirpCode(t, err, twirp.Unauthenticated)
}

func TestCreateDispatch_WrongRoom(t *testing.T) {
	svc := newTestAgentDispatchService()
	// admin grant for "otherroom", but request is for "testroom"
	_, err := svc.CreateDispatch(adminContext("otherroom"), &livekit.CreateAgentDispatchRequest{
		Room:      "testroom",
		AgentName: "test-agent",
	})
	requireTwirpCode(t, err, twirp.Unauthenticated)
}

func TestCreateDispatch_Success(t *testing.T) {
	svc := newTestAgentDispatchService()
	expected := &livekit.AgentDispatch{
		Id:        "AD_test",
		AgentName: "test-agent",
		Room:      "testroom",
	}
	svc.agentClient.CreateDispatchReturns(expected, nil)

	resp, err := svc.CreateDispatch(adminContext("testroom"), &livekit.CreateAgentDispatchRequest{
		Room:      "testroom",
		AgentName: "test-agent",
	})
	require.NoError(t, err)
	require.Equal(t, expected, resp)
	require.Equal(t, 1, svc.agentClient.CreateDispatchCallCount())
}

func TestCreateDispatch_WithMetadata(t *testing.T) {
	svc := newTestAgentDispatchService()
	expected := &livekit.AgentDispatch{
		Id:        "AD_test",
		AgentName: "test-agent",
		Room:      "testroom",
		Metadata:  "some-metadata",
	}
	svc.agentClient.CreateDispatchReturns(expected, nil)

	resp, err := svc.CreateDispatch(adminContext("testroom"), &livekit.CreateAgentDispatchRequest{
		Room:      "testroom",
		AgentName: "test-agent",
		Metadata:  "some-metadata",
	})
	require.NoError(t, err)
	require.Equal(t, expected, resp)

	// verify the dispatch passed to client has the metadata
	_, topic, dispatch, _ := svc.agentClient.CreateDispatchArgsForCall(0)
	require.NotEmpty(t, topic)
	require.Equal(t, "test-agent", dispatch.AgentName)
	require.Equal(t, "testroom", dispatch.Room)
	require.NotEmpty(t, dispatch.Id)
	require.Equal(t, "some-metadata", dispatch.Metadata)
}

func TestCreateDispatch_AutoCreateEnabled(t *testing.T) {
	svc := newTestAgentDispatchService()
	svc.allocator.AutoCreateEnabledReturns(true)
	svc.router.CreateRoomReturns(&livekit.Room{Name: "testroom"}, nil)
	svc.agentClient.CreateDispatchReturns(&livekit.AgentDispatch{Id: "AD_test"}, nil)

	resp, err := svc.CreateDispatch(adminContext("testroom"), &livekit.CreateAgentDispatchRequest{
		Room:      "testroom",
		AgentName: "test-agent",
	})
	require.NoError(t, err)
	require.NotNil(t, resp)

	// verify room allocation was performed
	require.Equal(t, 1, svc.allocator.SelectRoomNodeCallCount())
	_, roomName, nodeID := svc.allocator.SelectRoomNodeArgsForCall(0)
	require.Equal(t, livekit.RoomName("testroom"), roomName)
	require.Equal(t, livekit.NodeID(""), nodeID)

	// verify room creation was requested
	require.Equal(t, 1, svc.router.CreateRoomCallCount())
	_, createReq := svc.router.CreateRoomArgsForCall(0)
	require.Equal(t, "testroom", createReq.Name)
}

func TestCreateDispatch_AutoCreate_SelectRoomNodeFails(t *testing.T) {
	svc := newTestAgentDispatchService()
	svc.allocator.AutoCreateEnabledReturns(true)
	svc.allocator.SelectRoomNodeReturns(errors.New("node selection failed"))

	_, err := svc.CreateDispatch(adminContext("testroom"), &livekit.CreateAgentDispatchRequest{
		Room:      "testroom",
		AgentName: "test-agent",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "node selection failed")
	require.Equal(t, 0, svc.router.CreateRoomCallCount())
}

func TestCreateDispatch_AutoCreate_CreateRoomFails(t *testing.T) {
	svc := newTestAgentDispatchService()
	svc.allocator.AutoCreateEnabledReturns(true)
	svc.router.CreateRoomReturns(nil, errors.New("room creation failed"))

	_, err := svc.CreateDispatch(adminContext("testroom"), &livekit.CreateAgentDispatchRequest{
		Room:      "testroom",
		AgentName: "test-agent",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "room creation failed")
	require.Equal(t, 0, svc.agentClient.CreateDispatchCallCount())
}

func TestCreateDispatch_AutoCreateDisabled(t *testing.T) {
	svc := newTestAgentDispatchService()
	svc.allocator.AutoCreateEnabledReturns(false)
	svc.agentClient.CreateDispatchReturns(&livekit.AgentDispatch{Id: "AD_test"}, nil)

	_, err := svc.CreateDispatch(adminContext("testroom"), &livekit.CreateAgentDispatchRequest{
		Room:      "testroom",
		AgentName: "test-agent",
	})
	require.NoError(t, err)
	require.Equal(t, 0, svc.allocator.SelectRoomNodeCallCount())
	require.Equal(t, 0, svc.router.CreateRoomCallCount())
}

func TestCreateDispatch_ClientError(t *testing.T) {
	svc := newTestAgentDispatchService()
	svc.agentClient.CreateDispatchReturns(nil, errors.New("rpc error"))

	_, err := svc.CreateDispatch(adminContext("testroom"), &livekit.CreateAgentDispatchRequest{
		Room:      "testroom",
		AgentName: "test-agent",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "rpc error")
}

// --- DeleteDispatch ---
func TestDeleteDispatch_NilRequest(t *testing.T) {
	svc := newTestAgentDispatchService()
	_, err := svc.DeleteDispatch(adminContext("testroom"), nil)
	requireTwirpCode(t, err, twirp.InvalidArgument)
}

func TestDeleteDispatch_MissingRoom(t *testing.T) {
	svc := newTestAgentDispatchService()
	_, err := svc.DeleteDispatch(adminContext("testroom"), &livekit.DeleteAgentDispatchRequest{
		DispatchId: "AD_123",
	})
	requireTwirpCode(t, err, twirp.InvalidArgument)
}

func TestDeleteDispatch_NoPermission(t *testing.T) {
	svc := newTestAgentDispatchService()
	_, err := svc.DeleteDispatch(nonAdminContext(), &livekit.DeleteAgentDispatchRequest{
		Room:       "testroom",
		DispatchId: "AD_123",
	})
	requireTwirpCode(t, err, twirp.Unauthenticated)
}

func TestDeleteDispatch_Success(t *testing.T) {
	svc := newTestAgentDispatchService()
	expected := &livekit.AgentDispatch{
		Id:   "AD_123",
		Room: "testroom",
	}
	svc.agentClient.DeleteDispatchReturns(expected, nil)

	resp, err := svc.DeleteDispatch(adminContext("testroom"), &livekit.DeleteAgentDispatchRequest{
		Room:       "testroom",
		DispatchId: "AD_123",
	})
	require.NoError(t, err)
	require.Equal(t, expected, resp)
	require.Equal(t, 1, svc.agentClient.DeleteDispatchCallCount())
}

func TestDeleteDispatch_ClientError(t *testing.T) {
	svc := newTestAgentDispatchService()
	svc.agentClient.DeleteDispatchReturns(nil, errors.New("delete failed"))

	_, err := svc.DeleteDispatch(adminContext("testroom"), &livekit.DeleteAgentDispatchRequest{
		Room:       "testroom",
		DispatchId: "AD_123",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "delete failed")
}

// --- ListDispatch ---
func TestListDispatch_NilRequest(t *testing.T) {
	svc := newTestAgentDispatchService()
	_, err := svc.ListDispatch(adminContext("testroom"), nil)
	requireTwirpCode(t, err, twirp.InvalidArgument)
}

func TestListDispatch_MissingRoom(t *testing.T) {
	svc := newTestAgentDispatchService()
	_, err := svc.ListDispatch(adminContext("testroom"), &livekit.ListAgentDispatchRequest{
		DispatchId: "AD_123",
	})
	requireTwirpCode(t, err, twirp.InvalidArgument)
}

func TestListDispatch_NoPermission(t *testing.T) {
	svc := newTestAgentDispatchService()
	_, err := svc.ListDispatch(nonAdminContext(), &livekit.ListAgentDispatchRequest{
		Room: "testroom",
	})
	requireTwirpCode(t, err, twirp.Unauthenticated)
}

func TestListDispatch_Success(t *testing.T) {
	svc := newTestAgentDispatchService()
	expected := &livekit.ListAgentDispatchResponse{
		AgentDispatches: []*livekit.AgentDispatch{
			{Id: "AD_1", Room: "testroom", AgentName: "agent-1"},
			{Id: "AD_2", Room: "testroom", AgentName: "agent-2"},
		},
	}
	svc.agentClient.ListDispatchReturns(expected, nil)

	resp, err := svc.ListDispatch(adminContext("testroom"), &livekit.ListAgentDispatchRequest{
		Room: "testroom",
	})
	require.NoError(t, err)
	require.Equal(t, expected, resp)
	require.Equal(t, 1, svc.agentClient.ListDispatchCallCount())
}

func TestListDispatch_WithDispatchId(t *testing.T) {
	svc := newTestAgentDispatchService()
	expected := &livekit.ListAgentDispatchResponse{
		AgentDispatches: []*livekit.AgentDispatch{
			{Id: "AD_1", Room: "testroom", AgentName: "agent-1"},
		},
	}
	svc.agentClient.ListDispatchReturns(expected, nil)

	resp, err := svc.ListDispatch(adminContext("testroom"), &livekit.ListAgentDispatchRequest{
		Room:       "testroom",
		DispatchId: "AD_1",
	})
	require.NoError(t, err)
	require.Len(t, resp.AgentDispatches, 1)

	// verify the request was forwarded with the dispatch ID
	_, _, listReq, _ := svc.agentClient.ListDispatchArgsForCall(0)
	require.Equal(t, "AD_1", listReq.DispatchId)
}

func TestListDispatch_ClientError(t *testing.T) {
	svc := newTestAgentDispatchService()
	svc.agentClient.ListDispatchReturns(nil, errors.New("list failed"))

	_, err := svc.ListDispatch(adminContext("testroom"), &livekit.ListAgentDispatchRequest{
		Room: "testroom",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "list failed")
}

// helper
func requireTwirpCode(t *testing.T, err error, code twirp.ErrorCode) {
	t.Helper()
	require.Error(t, err)
	terr, ok := err.(twirp.Error)
	require.True(t, ok, "expected twirp.Error, got %T", err)
	require.Equal(t, code, terr.Code())
}
