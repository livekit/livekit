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
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
)

// protocolTestAllocator and protocolTestRouter are minimal stubs: the paths
// exercised below only need room validation to pass and are not meant to
// reach the router. neither implements routing.Router, so the node capacity
// block in ValidateConnectRequest is skipped.
type protocolTestAllocator struct{}

func (protocolTestAllocator) AutoCreateEnabled(context.Context) bool { return true }
func (protocolTestAllocator) SelectRoomNode(context.Context, livekit.RoomName, livekit.NodeID) error {
	return nil
}
func (protocolTestAllocator) CreateRoom(context.Context, *livekit.CreateRoomRequest, bool) (*livekit.Room, *livekit.RoomInternal, *livekit.CreateRoomRequest, bool, error) {
	return nil, nil, nil, false, nil
}
func (protocolTestAllocator) ValidateCreateRoom(context.Context, livekit.RoomName) error { return nil }

type protocolTestRouter struct{}

func (protocolTestRouter) CreateRoom(context.Context, *livekit.CreateRoomRequest) (*livekit.Room, error) {
	return nil, nil
}
func (protocolTestRouter) StartParticipantSignal(context.Context, livekit.RoomName, routing.ParticipantInit) (routing.StartParticipantSignalResults, error) {
	return routing.StartParticipantSignalResults{}, nil
}

func protocolTestGrants() *auth.ClaimGrants {
	return &auth.ClaimGrants{
		Identity: "test-participant",
		Video: &auth.VideoGrant{
			RoomJoin: true,
			Room:     "test-room",
		},
	}
}

func protocolTestRequest(protocol string, reconnect bool) *http.Request {
	form := "protocol=" + protocol
	if reconnect {
		form += "&reconnect=true&sid=PA_test"
	}

	req := httptest.NewRequest(http.MethodGet, "/rtc/v1?"+form, nil)
	return req.WithContext(WithGrants(req.Context(), protocolTestGrants(), "testkey"))
}

func newProtocolTestService(limits config.LimitConfig) *RTCService {
	conf := &config.Config{Limit: limits}
	return NewRTCService(conf, protocolTestAllocator{}, protocolTestRouter{}, nil)
}

func validateProtocol(t *testing.T, limits config.LimitConfig, protocol string, reconnect bool) (int, error) {
	t.Helper()

	sut := newProtocolTestService(limits)
	_, _, code, err := sut.validateInternal(logger.GetLogger(), protocolTestRequest(protocol, reconnect), false, false)
	return code, err
}

func TestMinClientProtocolRejectsOutdatedClient(t *testing.T) {
	code, err := validateProtocol(t, config.LimitConfig{MinClientProtocol: 17}, strconv.Itoa(16), false)

	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, code)
	// the error must tell the user what to do
	require.Contains(t, err.Error(), "below the configured minimum 17")
	require.Contains(t, err.Error(), "upgrade the client SDK")
}

func TestMinClientProtocolAcceptsCurrentClient(t *testing.T) {
	code, err := validateProtocol(t, config.LimitConfig{MinClientProtocol: 17}, strconv.Itoa(17), false)

	require.NoError(t, err)
	require.Equal(t, http.StatusOK, code)
}

func TestMinClientProtocolRejectsReconnect(t *testing.T) {
	// the reconnect flag comes from the client, so it must not exempt anyone
	// from the floor: an outdated client would otherwise skip the minimum by
	// setting it
	code, err := validateProtocol(t, config.LimitConfig{MinClientProtocol: 17}, strconv.Itoa(16), true)

	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, code)
}

func TestMinClientProtocolDisabledByDefault(t *testing.T) {
	// protocol "0" is what a client that does not report a version sends;
	// with no minimum configured it is accepted as before
	code, err := validateProtocol(t, config.LimitConfig{}, "0", false)

	require.NoError(t, err)
	require.Equal(t, http.StatusOK, code)
}

func TestMinClientProtocolRejectsUnreportedVersion(t *testing.T) {
	// a client that does not report a protocol cannot prove it meets the
	// minimum, so it is rejected
	code, err := validateProtocol(t, config.LimitConfig{MinClientProtocol: 17}, "0", false)

	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, code)
}

func TestCheckClientProtocol(t *testing.T) {
	require.True(t, config.LimitConfig{}.CheckClientProtocol(0))
	require.True(t, config.LimitConfig{MinClientProtocol: 17}.CheckClientProtocol(17))
	require.True(t, config.LimitConfig{MinClientProtocol: 17}.CheckClientProtocol(18))
	require.False(t, config.LimitConfig{MinClientProtocol: 17}.CheckClientProtocol(16))
	require.False(t, config.LimitConfig{MinClientProtocol: 17}.CheckClientProtocol(0))
}
