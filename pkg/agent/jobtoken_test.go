// Copyright 2026 LiveKit, Inc.

package agent_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/agent"
	"github.com/livekit/livekit-server/pkg/agent/testutils"
	protoagent "github.com/livekit/protocol/agent"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/psrpc"
)

const jobTokenClaims = `{"identity":"avatar","kind":"agent","video":{"roomJoin":true,"room":"other-room","canPublishData":false}}`

func TestCreateJobToken(t *testing.T) {
	// startJob registers a worker and runs a job on it.
	startJob := func(t *testing.T, server *testutils.TestServer) (*testutils.AgentWorker, *livekit.Job) {
		w := server.SimulateAgentWorker()
		assignments := w.JobAssignments.Observe()
		defer assignments.Stop()
		w.Register("job_token_agent", livekit.JobType_JT_ROOM)
		w.SimulateRoomJob("job-room")
		select {
		case a := <-assignments.Events():
			return w, a.Job
		case <-time.After(5 * time.Second):
			require.Fail(t, "job assignment timeout")
			return nil, nil
		}
	}

	request := func(t *testing.T, w *testutils.AgentWorker, req *livekit.CreateJobTokenRequest) *livekit.CreateJobTokenResponse {
		responses := w.CreateJobTokenResponses.Observe()
		defer responses.Stop()
		w.SendCreateJobToken(req)
		select {
		case res := <-responses.Events():
			require.Equal(t, req.RequestId, res.RequestId)
			return res
		case <-time.After(5 * time.Second):
			require.Fail(t, "job token response timeout")
			return nil
		}
	}

	t.Run("issues the token the job asks for", func(t *testing.T) {
		server := testutils.NewTestServer(psrpc.NewLocalMessageBus())
		t.Cleanup(server.Close)
		w, job := startJob(t, server)

		res := request(t, w, &livekit.CreateJobTokenRequest{RequestId: "r1", JobId: job.Id, Claims: jobTokenClaims})
		require.Empty(t, res.Error)

		v, err := auth.ParseAPIToken(res.Token)
		require.NoError(t, err)
		require.Equal(t, server.TestAPIKey, v.APIKey())
		claims, grants, err := v.Verify(server.TestAPISecret)
		require.NoError(t, err)

		want, err := protoagent.ParseJobTokenClaims(jobTokenClaims)
		require.NoError(t, err)
		require.Equal(t, want, grants, "the token carries the parsed claims and nothing else")
		require.WithinDuration(t, time.Now().Add(10*time.Minute), claims.ExpiresAt.Time, 5*time.Second)
		require.InDelta(t, claims.ExpiresAt.Unix(), res.ExpiresAt, 1)
	})

	t.Run("refuses a job the worker does not run", func(t *testing.T) {
		server := testutils.NewTestServer(psrpc.NewLocalMessageBus())
		t.Cleanup(server.Close)
		w, job := startJob(t, server)

		res := request(t, w, &livekit.CreateJobTokenRequest{RequestId: "r1", JobId: "AJ_unknown", Claims: jobTokenClaims})
		require.NotEmpty(t, res.Error)
		require.Empty(t, res.Token)

		// the connection stays usable after a refusal
		res = request(t, w, &livekit.CreateJobTokenRequest{RequestId: "r2", JobId: job.Id, Claims: jobTokenClaims})
		require.Empty(t, res.Error)
		require.NotEmpty(t, res.Token)
	})

	t.Run("refuses another worker's job", func(t *testing.T) {
		server := testutils.NewTestServer(psrpc.NewLocalMessageBus())
		t.Cleanup(server.Close)
		_, job := startJob(t, server)
		other, _ := startJob(t, server)

		res := request(t, other, &livekit.CreateJobTokenRequest{RequestId: "r1", JobId: job.Id, Claims: jobTokenClaims})
		require.NotEmpty(t, res.Error)
		require.Empty(t, res.Token)
	})

	t.Run("refuses claims a job token never carries", func(t *testing.T) {
		server := testutils.NewTestServer(psrpc.NewLocalMessageBus())
		t.Cleanup(server.Close)
		w, job := startJob(t, server)

		res := request(t, w, &livekit.CreateJobTokenRequest{RequestId: "r1", JobId: job.Id, Claims: `{"sha256":"hash"}`})
		require.Contains(t, res.Error, "claim not allowed: sha256")
		require.Empty(t, res.Token)

		res = request(t, w, &livekit.CreateJobTokenRequest{RequestId: "r2", JobId: job.Id, Claims: `{"video":{"roomJoin":true,"agent":true}}`})
		require.Contains(t, res.Error, "claim not allowed: video.agent")
		require.Empty(t, res.Token)
	})

	t.Run("drops claims it does not know and refuses malformed claims", func(t *testing.T) {
		server := testutils.NewTestServer(psrpc.NewLocalMessageBus())
		t.Cleanup(server.Close)
		w, job := startJob(t, server)

		for _, claims := range []string{
			`{"identity":"avatar","unknown":true}`,
			`{"video":{"roomJoin":true,"unknown":true}}`,
			`{"exp":1}`,
		} {
			res := request(t, w, &livekit.CreateJobTokenRequest{RequestId: claims, JobId: job.Id, Claims: claims})
			require.Empty(t, res.Error, claims)
			require.NotEmpty(t, res.Token, claims)
		}

		res := request(t, w, &livekit.CreateJobTokenRequest{RequestId: "bad", JobId: job.Id, Claims: `not json`})
		require.NotEmpty(t, res.Error)
		require.Empty(t, res.Token)
	})
}

type recordingSignalConn struct {
	nopSignalConn
	mu       sync.Mutex
	messages []*livekit.ServerMessage
}

func (c *recordingSignalConn) WriteServerMessage(m *livekit.ServerMessage) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.messages = append(c.messages, m)
	return 0, nil
}

func (c *recordingSignalConn) last() *livekit.CreateJobTokenResponse {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.messages[len(c.messages)-1].GetCreateJobToken()
}

func TestCreateJobTokenAuthorizer(t *testing.T) {
	conn := &recordingSignalConn{}
	w := agent.NewWorker(agent.MakeWorkerRegistration(), "key", "secret", conn, logger.GetLogger())
	w.AttachJob(&livekit.Job{Id: "AJ_job", Room: &livekit.Room{Name: "job-room"}})
	req := &livekit.CreateJobTokenRequest{RequestId: "r1", JobId: "AJ_job", Claims: jobTokenClaims}

	t.Run("signs the grants it returns", func(t *testing.T) {
		var gotJob *livekit.Job
		var gotGrants *auth.ClaimGrants
		require.NoError(t, w.CreateJobToken(req, func(job *livekit.Job, grants *auth.ClaimGrants) (*auth.ClaimGrants, error) {
			gotJob, gotGrants = job, grants
			return &auth.ClaimGrants{Identity: "narrowed"}, nil
		}))
		require.Equal(t, "AJ_job", gotJob.Id)
		want, err := protoagent.ParseJobTokenClaims(jobTokenClaims)
		require.NoError(t, err)
		require.Equal(t, want, gotGrants)

		res := conn.last()
		require.Empty(t, res.Error)
		v, err := auth.ParseAPIToken(res.Token)
		require.NoError(t, err)
		_, grants, err := v.Verify("secret")
		require.NoError(t, err)
		require.Equal(t, &auth.ClaimGrants{Identity: "narrowed"}, grants)
	})

	t.Run("its error refuses the request", func(t *testing.T) {
		require.NoError(t, w.CreateJobToken(req, func(*livekit.Job, *auth.ClaimGrants) (*auth.ClaimGrants, error) {
			return nil, errors.New("claim not allowed: video.roomJoin")
		}))
		res := conn.last()
		require.Equal(t, "r1", res.RequestId)
		require.Equal(t, "claim not allowed: video.roomJoin", res.Error)
		require.Empty(t, res.Token)
	})

	t.Run("does not run for a job the worker does not run", func(t *testing.T) {
		called := false
		require.NoError(t, w.CreateJobToken(&livekit.CreateJobTokenRequest{RequestId: "r2", JobId: "AJ_other", Claims: jobTokenClaims},
			func(*livekit.Job, *auth.ClaimGrants) (*auth.ClaimGrants, error) {
				called = true
				return nil, nil
			}))
		require.False(t, called)
		require.NotEmpty(t, conn.last().Error)
	})
}
