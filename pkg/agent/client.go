// Copyright 2024 LiveKit, Inc.
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

package agent

import (
	"context"
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/psrpc"
)

const (
	EnabledCacheTTL         = 1 * time.Minute
	RoomAgentTopic          = "room"
	PublisherAgentTopic     = "publisher"
	DefaultHandlerNamespace = ""

	CheckEnabledTimeout = 5 * time.Second
)

type Client interface {
	// LaunchJob starts a room or participant job on an agent.
	// it will launch a job once for each worker in each namespace
	LaunchJob(ctx context.Context, desc *JobRequest)
	Stop() error
}

type JobRequest struct {
	JobType livekit.JobType
	Room    *livekit.Room
	// only set for participant jobs
	Participant *livekit.ParticipantInfo
	Metadata    string
	Namespace   string
}

type agentClient struct {
	client rpc.AgentInternalClient

	mu sync.RWMutex
}

func NewAgentClient(bus psrpc.MessageBus) (Client, error) {
	client, err := rpc.NewAgentInternalClient(bus)
	if err != nil {
		return nil, err
	}

	c := &agentClient{
		client:  client,
		subDone: make(chan struct{}),
	}

	return c, nil
}

func (c *agentClient) LaunchJob(ctx context.Context, desc *JobRequest) error {
	_, err := c.client.JobRequest(ctx, ns, jobTypeTopic, &livekit.Job{
		Id:          utils.NewGuid(utils.AgentJobPrefix),
		Type:        desc.JobType,
		Room:        desc.Room,
		Participant: desc.Participant,
		Namespace:   desc.Namespace,
	})
	if err != nil {
		logger.Infow("failed to send job request", "error", err, "namespace", ns, "jobType", desc.JobType)
		return err
	}

	return nil
}
