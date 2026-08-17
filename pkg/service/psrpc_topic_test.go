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
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/livekit-server/pkg/service/servicefakes"
)

type recordingEgressServer struct {
	rpc.UnimplementedEgressInternalServer
	egressID string
}

func (s *recordingEgressServer) StartEgress(context.Context, *rpc.StartEgressRequest) (*livekit.EgressInfo, error) {
	return &livekit.EgressInfo{EgressId: s.egressID}, nil
}

func (*recordingEgressServer) StartEgressAffinity(context.Context, *rpc.StartEgressRequest) float32 {
	return 1
}

type recordingSIPServer struct {
	rpc.UnimplementedSIPInternalServer
	participantID string
}

func (s *recordingSIPServer) CreateSIPParticipant(context.Context, *rpc.InternalCreateSIPParticipantRequest) (*rpc.InternalCreateSIPParticipantResponse, error) {
	return &rpc.InternalCreateSIPParticipantResponse{
		ParticipantId:       s.participantID,
		ParticipantIdentity: "sip_test",
	}, nil
}

type recordingIOClient struct {
	service.IOClient
}

func (recordingIOClient) CreateEgress(context.Context, *livekit.EgressInfo) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

func TestEgressLauncherUsesConfiguredClusterIDForStartTopic(t *testing.T) {
	for _, test := range []struct {
		name      string
		clusterID string
		egressID  string
	}{
		{name: "legacy global topic", egressID: "EG_global"},
		{name: "configured cluster topic", clusterID: "staging", egressID: "EG_staging"},
	} {
		t.Run(test.name, func(t *testing.T) {
			bus := psrpc.NewLocalMessageBus()
			egressServer, err := rpc.NewEgressInternalServer(
				&recordingEgressServer{egressID: test.egressID},
				bus,
			)
			require.NoError(t, err)
			require.NoError(t, egressServer.RegisterStartEgressTopic(test.clusterID))
			t.Cleanup(egressServer.Shutdown)

			egressClient, err := rpc.NewEgressClient(rpc.ClientParams{Bus: bus})
			require.NoError(t, err)
			t.Cleanup(egressClient.Close)

			launcher := service.NewEgressLauncher(egressClient, recordingIOClient{}, nil, test.clusterID)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			info, err := launcher.StartEgress(ctx, &rpc.StartEgressRequest{
				RoomId: "RM_test",
				Request: &rpc.StartEgressRequest_Egress{
					Egress: &livekit.StartEgressRequest{RoomName: "room"},
				},
			})
			require.NoError(t, err)
			require.Equal(t, test.egressID, info.EgressId)
		})
	}
}

func TestSIPServiceUsesConfiguredClusterIDForCreateTopic(t *testing.T) {
	bus := psrpc.NewLocalMessageBus()
	sipServer, err := rpc.NewSIPInternalServer(
		&recordingSIPServer{participantID: "PA_staging"},
		bus,
	)
	require.NoError(t, err)
	require.NoError(t, sipServer.RegisterCreateSIPParticipantTopic("staging"))
	t.Cleanup(sipServer.Shutdown)

	sipClient, err := rpc.NewSIPClient(bus)
	require.NoError(t, err)
	t.Cleanup(sipClient.Close)

	svc := service.NewSIPService(
		&config.SIPConfig{},
		"NE_test",
		"staging",
		nil,
		sipClient,
		&servicefakes.FakeSIPStore{},
		nil,
		nil,
	)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ctx = service.WithGrants(ctx, &auth.ClaimGrants{
		SIP: &auth.SIPGrant{Call: true},
	}, "")

	info, err := svc.CreateSIPParticipant(ctx, &livekit.CreateSIPParticipantRequest{
		Trunk:     &livekit.SIPOutboundConfig{Hostname: "sip.example.com"},
		SipNumber: "15551234567",
		SipCallTo: "+15557654321",
		RoomName:  "room",
	})
	require.NoError(t, err)
	require.Equal(t, "PA_staging", info.ParticipantId)
}
