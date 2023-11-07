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
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"
)

const (
	RoomAgentTopic      = "room"
	PublisherAgentTopic = "publisher"
)

type AgentClient interface {
	CheckEnabled(ctx context.Context, req *rpc.CheckEnabledRequest) *rpc.CheckEnabledResponse
	JobRequest(ctx context.Context, job *livekit.Job)
}

type agentClient struct {
	client rpc.AgentInternalClient
}

func NewAgentClient(bus psrpc.MessageBus) (AgentClient, error) {
	client, err := rpc.NewAgentInternalClient(bus)
	if err != nil {
		return nil, err
	}
	return &agentClient{client: client}, nil
}

func (c *agentClient) CheckEnabled(ctx context.Context, req *rpc.CheckEnabledRequest) *rpc.CheckEnabledResponse {
	res := &rpc.CheckEnabledResponse{}
	resChan, err := c.client.CheckEnabled(ctx, req, psrpc.WithRequestTimeout(time.Second))
	if err != nil {
		return res
	}

	for r := range resChan {
		if r.Err != nil {
			continue
		}
		if r.Result.RoomEnabled {
			res.RoomEnabled = true
			if res.PublisherEnabled {
				return res
			}
		}
		if r.Result.PublisherEnabled {
			res.PublisherEnabled = true
			if res.RoomEnabled {
				return res
			}
		}
	}

	return res
}

func (c *agentClient) JobRequest(ctx context.Context, job *livekit.Job) {
	var topic string
	var logError bool
	switch job.Type {
	case livekit.JobType_JT_ROOM:
		topic = RoomAgentTopic
	case livekit.JobType_JT_PUBLISHER:
		topic = PublisherAgentTopic
		logError = true
	}

	_, err := c.client.JobRequest(ctx, topic, job)
	if err != nil && logError {
		logger.Warnw("agent job request failed", err)
	}
}
