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
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing/routingfakes"
	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/livekit-server/pkg/service/servicefakes"
)

func TestWHIPCreateWithAutoCreateDisabled(t *testing.T) {
	post := func(t *testing.T, grant *auth.VideoGrant) (int, *routingfakes.FakeRouter) {
		conf, err := config.NewConfig("", true, nil, nil)
		require.NoError(t, err)
		conf.Room.AutoCreate = false

		store := &servicefakes.FakeObjectStore{}
		store.LoadRoomReturns(nil, nil, service.ErrRoomNotFound)
		router := &routingfakes.FakeRouter{}
		router.GetNodeForRoomReturns(nil, errors.New("no node"))
		ra, err := service.NewRoomAllocator(conf, router, store)
		require.NoError(t, err)

		s, err := service.NewWHIPService(conf, router, ra, rpc.ClientParams{Bus: psrpc.NewLocalMessageBus()}, nil, nil)
		require.NoError(t, err)
		mux := http.NewServeMux()
		s.SetupRoutes(mux)

		r := httptest.NewRequest(http.MethodPost, "/whip/v1", strings.NewReader("v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\n"))
		r.Header.Set("Content-type", "application/sdp")
		r = r.WithContext(service.WithGrants(context.Background(), &auth.ClaimGrants{Identity: "publisher", Video: grant}, ""))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w.Code, router
	}

	t.Run("refuses a room that does not exist", func(t *testing.T) {
		code, router := post(t, &auth.VideoGrant{RoomJoin: true, Room: "deleted-room"})
		require.Equal(t, http.StatusNotFound, code)
		require.Zero(t, router.GetNodeForRoomCallCount())
	})

	t.Run("creates the room with a roomCreate grant", func(t *testing.T) {
		_, router := post(t, &auth.VideoGrant{RoomJoin: true, RoomCreate: true, Room: "new-room"})
		require.NotZero(t, router.GetNodeForRoomCallCount())
	})
}
