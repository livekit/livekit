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

	"github.com/gammazero/workerpool"
	"google.golang.org/protobuf/types/known/emptypb"

	serverutils "github.com/livekit/livekit-server/pkg/utils"
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
	LaunchJob(ctx context.Context, desc *JobDescription)
	Stop() error
}

type JobDescription struct {
	JobType livekit.JobType
	Room    *livekit.Room
	// only set for participant jobs
	Participant *livekit.ParticipantInfo
}

type agentClient struct {
	client rpc.AgentInternalClient

	mu sync.RWMutex

	// cache response to avoid constantly checking with controllers
	// cache is invalidated with AgentRegistered updates
	roomNamespaces      *serverutils.IncrementalDispatcher[string]
	publisherNamespaces *serverutils.IncrementalDispatcher[string]
	enabledExpiresAt    time.Time

	workers *workerpool.WorkerPool

	invalidateSub psrpc.Subscription[*emptypb.Empty]
	subDone       chan struct{}
}

func NewAgentClient(bus psrpc.MessageBus) (Client, error) {
	client, err := rpc.NewAgentInternalClient(bus)
	if err != nil {
		return nil, err
	}

	c := &agentClient{
		client:  client,
		workers: workerpool.New(50),
		subDone: make(chan struct{}),
	}

	sub, err := c.client.SubscribeWorkerRegistered(context.Background(), DefaultHandlerNamespace)
	if err != nil {
		return nil, err
	}

	c.invalidateSub = sub

	go func() {
		// invalidate cache
		for range sub.Channel() {
			c.mu.Lock()
			c.roomNamespaces = nil
			c.publisherNamespaces = nil
			c.mu.Unlock()
		}

		c.subDone <- struct{}{}
	}()

	return c, nil
}

func (c *agentClient) LaunchJob(ctx context.Context, desc *JobDescription) {
	roomNamespaces, publisherNamespaces, needsRefresh := c.getOrCreateDispatchers()

	if needsRefresh {
		go c.checkEnabled(ctx, roomNamespaces, publisherNamespaces)
	}

	target := roomNamespaces
	jobTypeTopic := RoomAgentTopic
	if desc.JobType == livekit.JobType_JT_PUBLISHER {
		target = publisherNamespaces
		jobTypeTopic = PublisherAgentTopic
	}

	target.ForEach(func(ns string) {
		c.workers.Submit(func() {
			_, err := c.client.JobRequest(ctx, ns, jobTypeTopic, &livekit.Job{
				Id:          utils.NewGuid(utils.AgentJobPrefix),
				Type:        desc.JobType,
				Room:        desc.Room,
				Participant: desc.Participant,
				Namespace:   ns,
			})
			if err != nil {
				logger.Errorw("failed to send job request", err, "namespace", ns, "jobType", jobTypeTopic)
			}
		})
	})
}

func (c *agentClient) getOrCreateDispatchers() (*serverutils.IncrementalDispatcher[string], *serverutils.IncrementalDispatcher[string], bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if time.Since(c.enabledExpiresAt) > EnabledCacheTTL || c.roomNamespaces == nil || c.publisherNamespaces == nil {
		c.roomNamespaces = serverutils.NewIncrementalDispatcher[string]()
		c.publisherNamespaces = serverutils.NewIncrementalDispatcher[string]()
		return c.roomNamespaces, c.publisherNamespaces, true
	}
	return c.roomNamespaces, c.publisherNamespaces, false
}

func (c *agentClient) checkEnabled(ctx context.Context, roomNamespaces, publisherNamespaces *serverutils.IncrementalDispatcher[string]) {
	defer roomNamespaces.Done()
	defer publisherNamespaces.Done()
	resChan, err := c.client.CheckEnabled(ctx, &rpc.CheckEnabledRequest{}, psrpc.WithRequestTimeout(CheckEnabledTimeout))
	if err != nil {
		logger.Errorw("failed to check enabled", err)
		return
	}

	roomNSMap := make(map[string]bool)
	publisherNSMap := make(map[string]bool)

	for r := range resChan {
		if r.Result.GetRoomEnabled() {
			for _, ns := range r.Result.GetNamespaces() {
				if _, ok := roomNSMap[ns]; !ok {
					roomNamespaces.Add(ns)
					roomNSMap[ns] = true
				}
			}
		}
		if r.Result.GetPublisherEnabled() {
			for _, ns := range r.Result.GetNamespaces() {
				if _, ok := publisherNSMap[ns]; !ok {
					publisherNamespaces.Add(ns)
					publisherNSMap[ns] = true
				}
			}
		}
	}
}

func (c *agentClient) Stop() error {
	_ = c.invalidateSub.Close()
	<-c.subDone
	return nil
}
