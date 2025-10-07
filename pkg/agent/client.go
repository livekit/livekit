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
	"fmt"
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
	ParticipantAgentTopic   = "participant"
	DefaultHandlerNamespace = ""

	CheckEnabledTimeout = 5 * time.Second
)

var jobTypeTopics = map[livekit.JobType]string{
	livekit.JobType_JT_ROOM:        RoomAgentTopic,
	livekit.JobType_JT_PUBLISHER:   PublisherAgentTopic,
	livekit.JobType_JT_PARTICIPANT: ParticipantAgentTopic,
}

type Client interface {
	// LaunchJob starts a room or participant job on an agent.
	// it will launch a job once for each worker in each namespace
	LaunchJob(ctx context.Context, desc *JobRequest) *serverutils.IncrementalDispatcher[*livekit.Job]
	TerminateJob(ctx context.Context, jobID string, reason rpc.JobTerminateReason) (*livekit.JobState, error)
	Stop() error
}

type JobRequest struct {
	DispatchId string
	JobType    livekit.JobType
	Room       *livekit.Room
	// only set for participant jobs
	Participant *livekit.ParticipantInfo
	Metadata    string
	AgentName   string
}

type agentClient struct {
	client rpc.AgentInternalClient
	config Config

	mu sync.RWMutex

	// cache response to avoid constantly checking with controllers
	// cache is invalidated with AgentRegistered updates
	roomNamespaces        *serverutils.IncrementalDispatcher[string] // deprecated
	publisherNamespaces   *serverutils.IncrementalDispatcher[string] // deprecated
	participantNamespaces *serverutils.IncrementalDispatcher[string] // deprecated
	roomAgentNames        *serverutils.IncrementalDispatcher[string]
	publisherAgentNames   *serverutils.IncrementalDispatcher[string]
	participantAgentNames *serverutils.IncrementalDispatcher[string]

	enabledExpiresAt time.Time

	workers *workerpool.WorkerPool

	invalidateSub psrpc.Subscription[*emptypb.Empty]
	subDone       chan struct{}
}

func NewAgentClient(bus psrpc.MessageBus, config Config) (Client, error) {
	client, err := rpc.NewAgentInternalClient(bus)
	if err != nil {
		return nil, err
	}

	c := &agentClient{
		client:  client,
		config:  config,
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
			c.participantNamespaces = nil
			c.roomAgentNames = nil
			c.publisherAgentNames = nil
			c.participantAgentNames = nil
			c.mu.Unlock()
		}

		c.subDone <- struct{}{}
	}()

	return c, nil
}

func (c *agentClient) LaunchJob(ctx context.Context, desc *JobRequest) *serverutils.IncrementalDispatcher[*livekit.Job] {
	var wg sync.WaitGroup
	ret := serverutils.NewIncrementalDispatcher[*livekit.Job]()
	defer func() {
		c.workers.Submit(func() {
			wg.Wait()
			ret.Done()
		})
	}()

	jobTypeTopic, ok := jobTypeTopics[desc.JobType]
	if !ok {
		return ret
	}

	dispatcher := c.getDispatcher(desc.AgentName, desc.JobType)

	if dispatcher == nil {
		logger.Infow("not dispatching agent job since no worker is available",
			"agentName", desc.AgentName,
			"jobType", desc.JobType,
			"room", desc.Room.Name,
			"roomID", desc.Room.Sid)
		return ret
	}

	dispatcher.ForEach(func(curNs string) {
		topic := GetAgentTopic(desc.AgentName, curNs)

		wg.Add(1)
		c.workers.Submit(func() {
			defer wg.Done()
			// The cached agent parameters do not provide the exact combination of available job type/agent name/namespace, so some of the JobRequest RPC may not trigger any worker
			job := &livekit.Job{
				Id:              utils.NewGuid(utils.AgentJobPrefix),
				DispatchId:      desc.DispatchId,
				Type:            desc.JobType,
				Room:            desc.Room,
				Participant:     desc.Participant,
				Namespace:       curNs,
				AgentName:       desc.AgentName,
				Metadata:        desc.Metadata,
				EnableRecording: c.config.EnableUserDataRecording,
			}
			resp, err := c.client.JobRequest(context.Background(), topic, jobTypeTopic, job)
			if err != nil {
				logger.Infow("failed to send job request", "error", err, "namespace", curNs, "jobType", desc.JobType, "agentName", desc.AgentName)
				return
			}
			job.State = resp.State
			ret.Add(job)
		})
	})

	return ret
}

func (c *agentClient) TerminateJob(ctx context.Context, jobID string, reason rpc.JobTerminateReason) (*livekit.JobState, error) {
	resp, err := c.client.JobTerminate(context.Background(), jobID, &rpc.JobTerminateRequest{
		JobId:  jobID,
		Reason: reason,
	})
	if err != nil {
		logger.Infow("failed to send job request", "error", err, "jobID", jobID)
		return nil, err
	}

	return resp.State, nil
}

func (c *agentClient) getDispatcher(agName string, jobType livekit.JobType) *serverutils.IncrementalDispatcher[string] {
	c.mu.Lock()

	if time.Since(c.enabledExpiresAt) > EnabledCacheTTL || c.roomNamespaces == nil ||
		c.publisherNamespaces == nil || c.participantNamespaces == nil || c.roomAgentNames == nil || c.publisherAgentNames == nil || c.participantAgentNames == nil {
		c.enabledExpiresAt = time.Now()
		c.roomNamespaces = serverutils.NewIncrementalDispatcher[string]()
		c.publisherNamespaces = serverutils.NewIncrementalDispatcher[string]()
		c.participantNamespaces = serverutils.NewIncrementalDispatcher[string]()
		c.roomAgentNames = serverutils.NewIncrementalDispatcher[string]()
		c.publisherAgentNames = serverutils.NewIncrementalDispatcher[string]()
		c.participantAgentNames = serverutils.NewIncrementalDispatcher[string]()

		go c.checkEnabled(c.roomNamespaces, c.publisherNamespaces, c.participantNamespaces, c.roomAgentNames, c.publisherAgentNames, c.participantAgentNames)
	}

	var target *serverutils.IncrementalDispatcher[string]
	var agentNames *serverutils.IncrementalDispatcher[string]
	switch jobType {
	case livekit.JobType_JT_ROOM:
		target = c.roomNamespaces
		agentNames = c.roomAgentNames
	case livekit.JobType_JT_PUBLISHER:
		target = c.publisherNamespaces
		agentNames = c.publisherAgentNames
	case livekit.JobType_JT_PARTICIPANT:
		target = c.participantNamespaces
		agentNames = c.participantAgentNames
	}
	c.mu.Unlock()

	if agName == "" {
		// if no agent name is given, we would need to dispatch backwards compatible mode
		// which means dispatching to each of the namespaces
		return target
	}

	done := make(chan *serverutils.IncrementalDispatcher[string], 1)
	c.workers.Submit(func() {
		agentNames.ForEach(func(ag string) {
			if ag == agName {
				select {
				case done <- target:
				default:
				}
			}
		})
		select {
		case done <- nil:
		default:
		}
	})

	return <-done
}

func (c *agentClient) checkEnabled(roomNamespaces, publisherNamespaces, participantNamespaces, roomAgentNames, publisherAgentNames, participantAgentNames *serverutils.IncrementalDispatcher[string]) {
	defer roomNamespaces.Done()
	defer publisherNamespaces.Done()
	defer participantNamespaces.Done()
	defer roomAgentNames.Done()
	defer publisherAgentNames.Done()
	defer participantAgentNames.Done()

	resChan, err := c.client.CheckEnabled(context.Background(), &rpc.CheckEnabledRequest{}, psrpc.WithRequestTimeout(CheckEnabledTimeout))
	if err != nil {
		logger.Errorw("failed to check enabled", err)
		return
	}

	roomNSMap := make(map[string]bool)
	publisherNSMap := make(map[string]bool)
	participantNSMap := make(map[string]bool)
	roomAgMap := make(map[string]bool)
	publisherAgMap := make(map[string]bool)
	participantAgMap := make(map[string]bool)

	for r := range resChan {
		if r.Result.GetRoomEnabled() {
			for _, ns := range r.Result.GetNamespaces() {
				if _, ok := roomNSMap[ns]; !ok {
					roomNamespaces.Add(ns)
					roomNSMap[ns] = true
				}
			}
			for _, ag := range r.Result.GetAgentNames() {
				if _, ok := roomAgMap[ag]; !ok {
					roomAgentNames.Add(ag)
					roomAgMap[ag] = true
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
			for _, ag := range r.Result.GetAgentNames() {
				if _, ok := publisherAgMap[ag]; !ok {
					publisherAgentNames.Add(ag)
					publisherAgMap[ag] = true
				}
			}
		}
		if r.Result.GetParticipantEnabled() {
			for _, ns := range r.Result.GetNamespaces() {
				if _, ok := participantNSMap[ns]; !ok {
					participantNamespaces.Add(ns)
					participantNSMap[ns] = true
				}
			}
			for _, ag := range r.Result.GetAgentNames() {
				if _, ok := participantAgMap[ag]; !ok {
					participantAgentNames.Add(ag)
					participantAgMap[ag] = true
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

func GetAgentTopic(agentName, namespace string) string {
	if agentName == "" {
		// Backward compatibility
		return namespace
	} else if namespace == "" {
		// Forward compatibility once the namespace field is removed from the worker SDK
		return agentName
	} else {
		return fmt.Sprintf("%s_%s", agentName, namespace)
	}
}
