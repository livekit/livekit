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
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
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

type recordingSignalConn struct {
	nopSignalConn
	sent []*livekit.ServerMessage
}

func (c *recordingSignalConn) WriteServerMessage(m *livekit.ServerMessage) (int, error) {
	c.sent = append(c.sent, m)
	return 0, nil
}

// jobs reported on a new connection can be terminated and updated there.
func TestHandleMigrateJob(t *testing.T) {
	conn := &recordingSignalConn{}
	w := agent.NewWorker(agent.WorkerRegistration{}, "key", "secret", conn, logger.GetLogger())

	require.NoError(t, w.HandleMigrateJob(&livekit.MigrateJobRequest{JobIds: []string{"AJ_terminated", "AJ_updated"}}))
	require.Equal(t, 2, w.RunningJobCount())

	state, err := w.TerminateJob("AJ_terminated", rpc.JobTerminateReason_TERMINATION_REQUESTED)
	require.NoError(t, err)
	require.Equal(t, livekit.JobStatus_JS_SUCCESS, state.Status)
	require.Len(t, conn.sent, 1)
	require.Equal(t, "AJ_terminated", conn.sent[0].GetTermination().GetJobId())

	state, err = w.UpdateJobStatus(&livekit.UpdateJobStatus{JobId: "AJ_updated", Status: livekit.JobStatus_JS_SUCCESS})
	require.NoError(t, err)
	require.Equal(t, livekit.JobStatus_JS_SUCCESS, state.Status)
	require.Zero(t, w.RunningJobCount())
}
