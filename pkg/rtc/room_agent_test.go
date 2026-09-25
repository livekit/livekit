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

package rtc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/rpc"

	"github.com/livekit/livekit-server/pkg/agent"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/rtc/types/typesfakes"
	"github.com/livekit/livekit-server/pkg/utils"
)

type agentJobTermination struct {
	id     string
	reason rpc.JobTerminateReason
}

type retryAgentClient struct {
	agent.Client
	terminations chan agentJobTermination
}

func (c *retryAgentClient) TerminateJob(_ context.Context, id string, reason rpc.JobTerminateReason) (*livekit.JobState, error) {
	c.terminations <- agentJobTermination{id, reason}
	return &livekit.JobState{ParticipantIdentity: "agent", Status: livekit.JobStatus_JS_SUCCESS}, nil
}

func newRetryParticipant(r *Room, jobID string) *typesfakes.FakeLocalParticipant {
	p := NewMockParticipant("agent", types.CurrentProtocol, false, false, r.LocalParticipantListener())
	p.ConnectedAtReturns(time.Now())
	p.IsAgentReturns(true)
	// The secondary transport can connect while the primary is still stalled.
	p.HasConnectedReturns(true)
	p.IsDependentReturns(true)
	p.ClaimGrantsReturns(&auth.ClaimGrants{Attributes: map[string]string{agent.AgentJobIDAttributeKey: jobID}})
	return p
}

func newAgentRetryTest(t *testing.T) (*Room, *typesfakes.FakeLocalParticipant, *agentJob, <-chan agentJobTermination) {
	t.Helper()
	r := newRoomWithParticipants(t, testRoomOpts{})
	t.Cleanup(func() { r.Close(types.RoomCloseReasonUnknown) })
	client := &retryAgentClient{terminations: make(chan agentJobTermination, 10)}
	r.agentClient = client
	p := newRetryParticipant(r, "AJ_job")
	require.NoError(t, r.Join(p, nil, &ParticipantOptions{}, nil))
	job := newAgentJob(&livekit.Job{Id: "AJ_job", DispatchId: "AD_dispatch", State: &livekit.JobState{
		Status: livekit.JobStatus_JS_RUNNING, ParticipantIdentity: string(p.Identity()),
	}})
	r.lock.Lock()
	r.agentParticpants[p.Identity()] = job
	r.agentDispatches[job.DispatchId] = newAgentDispatch(&livekit.AgentDispatch{
		Id: job.DispatchId, State: &livekit.AgentDispatchState{Jobs: []*livekit.Job{job.Job}},
	})
	r.lock.Unlock()
	return r, p, job, client.terminations
}

func requireJobRunning(t *testing.T, r *Room, job *agentJob, done <-chan struct{}) {
	t.Helper()
	r.lock.RLock()
	current := r.agentParticpants["agent"]
	r.lock.RUnlock()
	require.Same(t, job, current)
	select {
	case <-done:
		t.Fatal("job was marked as having left the room")
	default:
	}
}

func requireJobTerminated(t *testing.T, calls <-chan agentJobTermination, reason rpc.JobTerminateReason) {
	t.Helper()
	select {
	case call := <-calls:
		require.Equal(t, "AJ_job", call.id)
		require.Equal(t, reason, call.reason)
	case <-time.After(time.Second):
		t.Fatal("job was not terminated")
	}
}

func TestAgentInitialConnectRetry(t *testing.T) {
	for _, reason := range []types.ParticipantCloseReason{
		types.ParticipantCloseReasonDuplicateIdentity,
		types.ParticipantCloseReasonPeerConnectionDisconnected,
	} {
		t.Run(reason.String(), func(t *testing.T) {
			r, old, job, calls := newAgentRetryTest(t)
			done := job.done
			r.RemoveParticipant(old.Identity(), old.ID(), reason)
			requireJobRunning(t, r, job, done)
			r.lock.RLock()
			retry := job.connectRetry
			r.lock.RUnlock()
			require.NotNil(t, retry)

			p := newRetryParticipant(r, job.Id)
			require.NoError(t, r.Join(p, nil, &ParticipantOptions{}, nil))
			// A late callback for the original SID must not remove the replacement.
			r.RemoveParticipant(old.Identity(), old.ID(), types.ParticipantCloseReasonPeerConnectionDisconnected)
			require.Same(t, p, r.GetParticipant(p.Identity()))
			requireJobRunning(t, r, job, done)

			p.HasConnectedReturns(true)
			p.ActiveAtReturns(time.Now())
			p.StateReturns(livekit.ParticipantInfo_ACTIVE)
			r.onStateChange(p)
			// Even a callback queued before Stop must leave the new session alone.
			r.expireAgentConnectRetry(p.Identity(), job, retry)
			requireJobRunning(t, r, job, done)
			require.Same(t, p, r.GetParticipant(p.Identity()))
			r.RemoveParticipant(p.Identity(), p.ID(), types.ParticipantCloseReasonClientRequestLeave)
			requireJobTerminated(t, calls, rpc.JobTerminateReason_AGENT_LEFT_ROOM)
			select {
			case <-done:
			default:
				t.Fatal("final departure did not close job notification")
			}
		})
	}
}

func TestAgentRetryEligibility(t *testing.T) {
	cases := []struct {
		name   string
		change func(*Room, *typesfakes.FakeLocalParticipant, *agentJob)
		reason types.ParticipantCloseReason
	}{
		{"already connected", func(_ *Room, p *typesfakes.FakeLocalParticipant, _ *agentJob) { p.ActiveAtReturns(time.Now()) }, types.ParticipantCloseReasonDuplicateIdentity},
		{"legacy token", func(_ *Room, p *typesfakes.FakeLocalParticipant, _ *agentJob) {
			p.ClaimGrantsReturns(&auth.ClaimGrants{})
		}, types.ParticipantCloseReasonDuplicateIdentity},
		{"not an agent", func(_ *Room, p *typesfakes.FakeLocalParticipant, _ *agentJob) { p.IsAgentReturns(false) }, types.ParticipantCloseReasonDuplicateIdentity},
		{"job ended", func(r *Room, _ *typesfakes.FakeLocalParticipant, j *agentJob) {
			r.lock.Lock()
			j.State.Status = livekit.JobStatus_JS_FAILED
			r.lock.Unlock()
		}, types.ParticipantCloseReasonDuplicateIdentity},
		{"dispatch deleted", func(r *Room, _ *typesfakes.FakeLocalParticipant, j *agentJob) {
			r.lock.Lock()
			delete(r.agentDispatches, j.DispatchId)
			r.lock.Unlock()
		}, types.ParticipantCloseReasonPeerConnectionDisconnected},
		{"explicit leave", nil, types.ParticipantCloseReasonClientRequestLeave},
		{"explicit removal", nil, types.ParticipantCloseReasonServiceRequestRemoveParticipant},
		{"join failed", nil, types.ParticipantCloseReasonJoinFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, p, job, calls := newAgentRetryTest(t)
			if tc.change != nil {
				tc.change(r, p, job)
			}
			r.RemoveParticipant(p.Identity(), p.ID(), tc.reason)
			requireJobTerminated(t, calls, rpc.JobTerminateReason_AGENT_LEFT_ROOM)
			r.lock.RLock()
			require.Nil(t, r.agentParticpants[p.Identity()])
			require.Nil(t, job.connectRetry)
			r.lock.RUnlock()
		})
	}
}

func TestAgentRetryRejectsJobInheritance(t *testing.T) {
	for _, jobID := range []string{"AJ_other", ""} {
		t.Run(jobID, func(t *testing.T) {
			r, old, job, calls := newAgentRetryTest(t)
			r.RemoveParticipant(old.Identity(), old.ID(), types.ParticipantCloseReasonDuplicateIdentity)
			r.lock.RLock()
			retry := job.connectRetry
			r.lock.RUnlock()
			p := newRetryParticipant(r, jobID)
			require.NoError(t, r.Join(p, nil, &ParticipantOptions{}, nil))
			requireJobTerminated(t, calls, rpc.JobTerminateReason_AGENT_LEFT_ROOM)
			r.expireAgentConnectRetry(p.Identity(), job, retry)
			require.Same(t, p, r.GetParticipant(p.Identity()))
		})
	}
}

func TestAgentRetryExpires(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(map[bool]string{false: "no replacement", true: "replacement never connected"}[replacement], func(t *testing.T) {
			r, old, job, calls := newAgentRetryTest(t)
			done := job.done
			r.RemoveParticipant(old.Identity(), old.ID(), types.ParticipantCloseReasonPeerConnectionDisconnected)
			if replacement {
				p := newRetryParticipant(r, job.Id)
				require.NoError(t, r.Join(p, nil, &ParticipantOptions{}, nil))
			}
			r.lock.Lock()
			job.connectRetry.timer.Reset(time.Millisecond)
			r.lock.Unlock()
			requireJobTerminated(t, calls, rpc.JobTerminateReason_AGENT_LEFT_ROOM)
			require.Eventually(t, func() bool { return r.GetParticipant(old.Identity()) == nil }, time.Second, time.Millisecond)
			select {
			case <-done:
			default:
				t.Fatal("expired job notification still open")
			}
			r.lock.RLock()
			require.Nil(t, r.agentParticpants[old.Identity()])
			r.lock.RUnlock()
		})
	}
}

func TestAgentRetriesDoNotExtendDeadline(t *testing.T) {
	r, old, job, calls := newAgentRetryTest(t)
	r.RemoveParticipant(old.Identity(), old.ID(), types.ParticipantCloseReasonDuplicateIdentity)
	r.lock.RLock()
	retry := job.connectRetry
	r.lock.RUnlock()
	p := newRetryParticipant(r, job.Id)
	require.NoError(t, r.Join(p, nil, &ParticipantOptions{}, nil))
	r.RemoveParticipant(p.Identity(), p.ID(), types.ParticipantCloseReasonPeerConnectionDisconnected)
	r.lock.RLock()
	require.Same(t, retry, job.connectRetry)
	r.lock.RUnlock()
	r.expireAgentConnectRetry(p.Identity(), job, retry)
	requireJobTerminated(t, calls, rpc.JobTerminateReason_AGENT_LEFT_ROOM)
}

func TestAgentRetryFailedJoin(t *testing.T) {
	r, old, job, calls := newAgentRetryTest(t)
	r.RemoveParticipant(old.Identity(), old.ID(), types.ParticipantCloseReasonDuplicateIdentity)
	p := newRetryParticipant(r, job.Id)
	p.SendJoinResponseReturns(errors.New("signal closed"))
	require.Error(t, r.Join(p, nil, &ParticipantOptions{}, nil))
	// The service removes failed participants through their close callback.
	r.RemoveParticipant(p.Identity(), p.ID(), types.ParticipantCloseReasonJoinFailed)
	requireJobTerminated(t, calls, rpc.JobTerminateReason_AGENT_LEFT_ROOM)
	r.lock.RLock()
	require.Nil(t, job.connectRetry)
	r.lock.RUnlock()
}

func TestAgentRetryRoomAndDispatchDeletion(t *testing.T) {
	t.Run("room", func(t *testing.T) {
		r, p, job, calls := newAgentRetryTest(t)
		r.RemoveParticipant(p.Identity(), p.ID(), types.ParticipantCloseReasonDuplicateIdentity)
		r.Close(types.RoomCloseReasonUnknown)
		requireJobTerminated(t, calls, rpc.JobTerminateReason_AGENT_LEFT_ROOM)
		r.lock.RLock()
		require.Nil(t, job.connectRetry)
		require.Nil(t, r.agentParticpants[p.Identity()])
		r.lock.RUnlock()
	})
	t.Run("dispatch", func(t *testing.T) {
		r, p, job, calls := newAgentRetryTest(t)
		done := job.done
		r.RemoveParticipant(p.Identity(), p.ID(), types.ParticipantCloseReasonPeerConnectionDisconnected)
		_, err := r.DeleteAgentDispatch(job.DispatchId)
		require.NoError(t, err)
		requireJobTerminated(t, calls, rpc.JobTerminateReason_TERMINATION_REQUESTED)
		select {
		case <-done:
		default:
			t.Fatal("deleted job notification still open")
		}
		r.lock.RLock()
		require.Nil(t, job.connectRetry)
		require.Nil(t, r.agentParticpants[p.Identity()])
		r.lock.RUnlock()
	})
	t.Run("dispatch with pending replacement", func(t *testing.T) {
		r, old, job, calls := newAgentRetryTest(t)
		done := job.done
		r.RemoveParticipant(old.Identity(), old.ID(), types.ParticipantCloseReasonPeerConnectionDisconnected)
		p := newRetryParticipant(r, job.Id)
		require.NoError(t, r.Join(p, nil, &ParticipantOptions{}, nil))
		_, err := r.DeleteAgentDispatch(job.DispatchId)
		require.NoError(t, err)
		requireJobTerminated(t, calls, rpc.JobTerminateReason_TERMINATION_REQUESTED)
		// The replacement keeps the mapping so dispatch termination can wait for it to leave.
		requireJobRunning(t, r, job, done)
		r.lock.RLock()
		require.Nil(t, job.connectRetry)
		r.lock.RUnlock()

		// With the dispatch gone, a pre-ACTIVE failure must terminate instead of re-arming a retry.
		r.RemoveParticipant(p.Identity(), p.ID(), types.ParticipantCloseReasonPeerConnectionDisconnected)
		requireJobTerminated(t, calls, rpc.JobTerminateReason_AGENT_LEFT_ROOM)
		select {
		case <-done:
		default:
			t.Fatal("deleted job notification still open")
		}
		r.lock.RLock()
		require.Nil(t, job.connectRetry)
		require.Nil(t, r.agentParticpants[p.Identity()])
		r.lock.RUnlock()
	})
}

func TestOldAgentDepartureDoesNotTerminateNewAssignment(t *testing.T) {
	r, p, job, _ := newAgentRetryTest(t)
	done := job.done
	p.ClaimGrantsReturns(&auth.ClaimGrants{Attributes: map[string]string{agent.AgentJobIDAttributeKey: "AJ_old"}})
	r.RemoveParticipant(p.Identity(), p.ID(), types.ParticipantCloseReasonDuplicateIdentity)
	requireJobRunning(t, r, job, done)
}

func TestAgentRetryConnectedBeforeStateNotification(t *testing.T) {
	r, old, job, _ := newAgentRetryTest(t)
	done := job.done
	r.RemoveParticipant(old.Identity(), old.ID(), types.ParticipantCloseReasonDuplicateIdentity)
	r.lock.RLock()
	retry := job.connectRetry
	r.lock.RUnlock()
	p := newRetryParticipant(r, job.Id)
	require.NoError(t, r.Join(p, nil, &ParticipantOptions{}, nil))
	// ACTIVE's listener runs asynchronously. Expiry must also inspect ActiveAt.
	p.ActiveAtReturns(time.Now())
	r.expireAgentConnectRetry(p.Identity(), job, retry)
	requireJobRunning(t, r, job, done)
	require.Same(t, p, r.GetParticipant(p.Identity()))
}

type retryAgentStore struct{ AgentStore }

func (retryAgentStore) StoreAgentJob(context.Context, *livekit.Job) error { return nil }

func TestAgentRetryReplacedByNewAssignment(t *testing.T) {
	r, old, job, calls := newAgentRetryTest(t)
	r.agentStore = retryAgentStore{}
	r.RemoveParticipant(old.Identity(), old.ID(), types.ParticipantCloseReasonDuplicateIdentity)
	r.lock.RLock()
	retry := job.connectRetry
	ad := r.agentDispatches[job.DispatchId]
	r.lock.RUnlock()
	next := &livekit.Job{Id: "AJ_new", DispatchId: job.DispatchId, State: &livekit.JobState{
		Status: livekit.JobStatus_JS_RUNNING, ParticipantIdentity: string(old.Identity()),
	}}
	jobs := utils.NewIncrementalDispatcher[*livekit.Job]()
	jobs.Add(next)
	jobs.Done()
	r.handleNewJobs(ad.AgentDispatch, jobs)
	requireJobTerminated(t, calls, rpc.JobTerminateReason_AGENT_LEFT_ROOM)
	p := newRetryParticipant(r, next.Id)
	require.NoError(t, r.Join(p, nil, &ParticipantOptions{}, nil))
	r.expireAgentConnectRetry(p.Identity(), job, retry)
	require.Same(t, p, r.GetParticipant(p.Identity()))
	r.lock.RLock()
	require.Equal(t, next.Id, r.agentParticpants[p.Identity()].Id)
	require.Nil(t, job.connectRetry)
	r.lock.RUnlock()
}

func TestAgentRetryDoesNotRestartInitialJoinTimeout(t *testing.T) {
	r, p, _, calls := newAgentRetryTest(t)
	// A failure at the end of the original join window must not grant another minute.
	p.ConnectedAtReturns(time.Now().Add(-participantJoinTimeout))
	r.RemoveParticipant(p.Identity(), p.ID(), types.ParticipantCloseReasonPeerConnectionDisconnected)
	requireJobTerminated(t, calls, rpc.JobTerminateReason_AGENT_LEFT_ROOM)
}
