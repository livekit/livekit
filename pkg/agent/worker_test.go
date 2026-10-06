// Copyright 2026 LiveKit, Inc.

package agent_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/agent"
	"github.com/livekit/livekit-server/pkg/agent/endpoint/wire"
	"github.com/livekit/protocol/livekit"
)

type nopSignalConn struct{}

func (nopSignalConn) WriteServerMessage(*livekit.ServerMessage) (int, error)  { return 0, nil }
func (nopSignalConn) ReadWorkerMessage() (*livekit.WorkerMessage, int, error) { return nil, 0, nil }
func (nopSignalConn) SetReadDeadline(time.Time) error                         { return nil }
func (nopSignalConn) Close() error                                            { return nil }
func (nopSignalConn) CloseWithReason(string) error                            { return nil }

// the name is percent-encoded into a URL path segment.
func TestHandleRegisterEndpointAgentNames(t *testing.T) {
	cases := []struct {
		name      string
		agentName string
		ok        bool
	}{
		{"plain", "test-agent", true},
		{"underscore", "my_agent", true},
		{"spaces", "LODHA Vayam Agent", true},
		{"colon and star", "prod:*", true},
		{"at sign", "charlie@v1.42.0", true},
		{"slash", "models/gemma/v2", true},
		{"brackets", "Nathan [Elara Agent]", true},
		{"non ascii", "Sehhaty الصحة", true},
		{"past the old 64 byte cap", strings.Repeat("n", 77), true},

		{"empty", "", false},
		{"unnamed sentinel", "_", false},
		{"dot", ".", false},
		{"dot dot", "..", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := agent.NewWorkerRegisterer(nopSignalConn{}, &livekit.ServerInfo{}, agent.WorkerRegistration{},
				agent.EndpointRegisterHandler)

			err := h.HandleRegister(&livekit.RegisterWorkerRequest{
				Type:             livekit.JobType_JT_ROOM,
				AgentName:        c.agentName,
				Endpoints:        []*livekit.AgentHttp_AgentEndpoint{{Path: "/hook", Methods: []string{"GET"}}},
				InstanceId:       "AEI_test",
				EndpointProtocol: wire.CurrentProtocol,
			})
			if c.ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

// the reserved names constrain only workers that declare endpoints.
func TestHandleRegisterWithoutEndpointsIgnoresReservedNames(t *testing.T) {
	for _, name := range []string{"", "_", ".", ".."} {
		h := agent.NewWorkerRegisterer(nopSignalConn{}, &livekit.ServerInfo{}, agent.WorkerRegistration{}, agent.EndpointRegisterHandler)
		require.NoError(t, h.HandleRegister(&livekit.RegisterWorkerRequest{
			Type:      livekit.JobType_JT_ROOM,
			AgentName: name,
		}), name)
	}
}
