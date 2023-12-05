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
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/psrpc"
)

func init() {
	prometheus.Init("node", livekit.NodeType_CONTROLLER, "test")
}

func TestSignal(t *testing.T) {
	t.Run("messages are delivered", func(t *testing.T) {
		bus := psrpc.NewLocalMessageBus()
		cfg := config.SignalRelayConfig{
			Enabled:          false,
			RetryTimeout:     30 * time.Second,
			MinRetryInterval: 500 * time.Millisecond,
			MaxRetryInterval: 5 * time.Second,
			StreamBufferSize: 1000,
		}

		reqMessageIn := &livekit.SignalRequest{
			Message: &livekit.SignalRequest_Ping{Ping: 123},
		}
		resMessageIn := &livekit.SignalResponse{
			Message: &livekit.SignalResponse_Pong{Pong: 321},
		}

		var reqMessageOut proto.Message
		var resErr error
		done := make(chan struct{})

		client, err := routing.NewSignalClient(livekit.NodeID("node0"), bus, cfg)
		require.NoError(t, err)

		server, err := NewSignalServer(livekit.NodeID("node1"), "region", bus, cfg, func(
			ctx context.Context,
			roomName livekit.RoomName,
			pi routing.ParticipantInit,
			connectionID livekit.ConnectionID,
			requestSource routing.MessageSource,
			responseSink routing.MessageSink,
		) error {
			go func() {
				reqMessageOut = <-requestSource.ReadChan()
				resErr = responseSink.WriteMessage(resMessageIn)
				responseSink.Close()
				close(done)
			}()
			return nil
		})
		require.NoError(t, err)

		err = server.Start()
		require.NoError(t, err)

		_, reqSink, resSource, err := client.StartParticipantSignal(
			context.Background(),
			livekit.RoomName("room1"),
			routing.ParticipantInit{},
			livekit.NodeID("node1"),
		)
		require.NoError(t, err)

		err = reqSink.WriteMessage(reqMessageIn)
		require.NoError(t, err)

		<-done
		require.True(t, proto.Equal(reqMessageIn, reqMessageOut), "req message should match %s %s", protojson.Format(reqMessageIn), protojson.Format(reqMessageOut))
		require.NoError(t, resErr)

		resMessageOut := <-resSource.ReadChan()
		require.True(t, proto.Equal(resMessageIn, resMessageOut), "res message should match %s %s", protojson.Format(resMessageIn), protojson.Format(resMessageOut))
	})

	t.Run("messages are delivered when session handler fails", func(t *testing.T) {
		bus := psrpc.NewLocalMessageBus()
		cfg := config.SignalRelayConfig{
			Enabled:          false,
			RetryTimeout:     30 * time.Second,
			MinRetryInterval: 500 * time.Millisecond,
			MaxRetryInterval: 5 * time.Second,
			StreamBufferSize: 1000,
		}

		resMessageIn := &livekit.SignalResponse{
			Message: &livekit.SignalResponse_Pong{Pong: 321},
		}

		var resErr error
		done := make(chan struct{})

		client, err := routing.NewSignalClient(livekit.NodeID("node0"), bus, cfg)
		require.NoError(t, err)

		server, err := NewSignalServer(livekit.NodeID("node1"), "region", bus, cfg, func(
			ctx context.Context,
			roomName livekit.RoomName,
			pi routing.ParticipantInit,
			connectionID livekit.ConnectionID,
			requestSource routing.MessageSource,
			responseSink routing.MessageSink,
		) error {
			defer close(done)
			resErr = responseSink.WriteMessage(resMessageIn)
			return errors.New("start session failed")
		})
		require.NoError(t, err)

		err = server.Start()
		require.NoError(t, err)

		_, _, resSource, err := client.StartParticipantSignal(
			context.Background(),
			livekit.RoomName("room1"),
			routing.ParticipantInit{},
			livekit.NodeID("node1"),
		)
		require.NoError(t, err)

		<-done
		require.NoError(t, resErr)

		resMessageOut := <-resSource.ReadChan()
		require.True(t, proto.Equal(resMessageIn, resMessageOut), "res message should match %s %s", protojson.Format(resMessageIn), protojson.Format(resMessageOut))
	})
}
