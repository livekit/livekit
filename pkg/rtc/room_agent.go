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
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/rpc"

	"github.com/livekit/livekit-server/pkg/agent"
	"github.com/livekit/livekit-server/pkg/rtc/types"
)

type agentConnectRetry struct {
	timer *time.Timer
}

func (j *agentJob) matchesParticipant(p types.LocalParticipant) bool {
	grants := p.ClaimGrants()
	return p.IsAgent() && grants != nil && j.Id != "" &&
		grants.Attributes[agent.AgentJobIDAttributeKey] == j.Id
}

func (j *agentJob) stopConnectRetry() {
	if j.connectRetry != nil {
		j.connectRetry.timer.Stop()
		j.connectRetry = nil
	}
}

// Called with Room.lock held. Only an unconnected participant from a live,
// server-assigned job is eligible; explicit leaves and removals still terminate it.
func (r *Room) deferAgentJobTerminationLocked(p types.LocalParticipant, job *agentJob, reason types.ParticipantCloseReason) bool {
	if job == nil || !p.ActiveAt().IsZero() || !job.matchesParticipant(p) || r.IsClosed() ||
		r.agentDispatches[job.DispatchId] == nil || agent.JobStatusIsEnded(job.GetState().GetStatus()) {
		return false
	}
	if reason != types.ParticipantCloseReasonDuplicateIdentity && reason != types.ParticipantCloseReasonPeerConnectionDisconnected {
		return false
	}
	if job.connectRetry == nil {
		retry := &agentConnectRetry{}
		job.connectRetry = retry
		// A retry uses the original participant's join deadline, not a fresh
		// timeout on every failed transport or replacement participant.
		retry.timer = time.AfterFunc(time.Until(p.ConnectedAt().Add(participantJoinTimeout)), func() {
			r.expireAgentConnectRetry(p.Identity(), job, retry)
		})
	}
	return true
}

func (r *Room) expireAgentConnectRetry(identity livekit.ParticipantIdentity, job *agentJob, retry *agentConnectRetry) {
	r.lock.Lock()
	// A stopped timer may already have queued its callback. It must not affect a
	// later retry, a different job, or a participant that has since connected.
	if r.agentParticpants[identity] != job || job.connectRetry != retry {
		r.lock.Unlock()
		return
	}
	p := r.participants[identity]
	if p != nil && job.matchesParticipant(p) && !p.ActiveAt().IsZero() {
		job.stopConnectRetry()
		r.lock.Unlock()
		return
	}
	r.deleteAgentJobLocked(identity, job)
	r.lock.Unlock()

	r.terminateAgentJob(identity, job)
	if p != nil && job.matchesParticipant(p) {
		r.RemoveParticipant(identity, p.ID(), types.ParticipantCloseReasonJoinTimeout)
	}
}

func (r *Room) deleteAgentJobLocked(identity livekit.ParticipantIdentity, job *agentJob) {
	delete(r.agentParticpants, identity)
	job.stopConnectRetry()
}

func (r *Room) terminateAgentJob(identity livekit.ParticipantIdentity, job *agentJob) {
	job.participantLeft()
	go func() {
		_, err := r.agentClient.TerminateJob(context.Background(), job.Id, rpc.JobTerminateReason_AGENT_LEFT_ROOM)
		if err != nil {
			r.logger.Infow("failed sending TerminateJob RPC", "error", err, "jobID", job.Id, "participant", identity)
		}
	}()
}
