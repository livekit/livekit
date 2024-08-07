package agent_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/agent"
	"github.com/livekit/livekit-server/pkg/agent/testutil"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/utils/guid"
	"github.com/livekit/protocol/utils/must"
	"github.com/livekit/psrpc"
)

func TestAgent(t *testing.T) {
	t.Run("dispatched jobs are assigned to a worker", func(t *testing.T) {
		bus := psrpc.NewLocalMessageBus()

		client := must.Get(rpc.NewAgentInternalClient(bus))
		server := testutil.NewTestServer(bus)
		t.Cleanup(server.Close)

		worker := server.SimulateAgentWorker()
		worker.Register("test", livekit.JobType_JT_ROOM)
		jobAssignments := worker.JobAssignments.Observe()

		job := &livekit.Job{
			Id:         guid.New(guid.AgentJobPrefix),
			DispatchId: guid.New(guid.AgentDispatchPrefix),
			Type:       livekit.JobType_JT_ROOM,
			Room:       &livekit.Room{},
			AgentName:  "test",
		}
		client.JobRequest(context.Background(), "test", agent.RoomAgentTopic, job)

		select {
		case a := <-jobAssignments.Events():
			require.EqualValues(t, job.Id, a.Job.Id)
		case <-time.After(time.Second):
			require.Fail(t, "job assignment timeout")
		}
	})
}
