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
	"google.golang.org/protobuf/types/known/emptypb"
)

var resolvedFalsePromise = utils.NewResolvedPromise[bool](false, nil)
var resolvedEmptyPromise = utils.NewResolvedPromise[[]string](make([]string, 0), nil)

const (
	EnabledCacheTTL     = 3 * time.Minute
	RoomAgentTopic      = "room"
	PublisherAgentTopic = "publisher"

	CheckEnabledTimeout = 5 * time.Second
)

type AgentClient interface {
	CheckEnabledPromise(ctx context.Context, req *rpc.CheckEnabledRequest) CheckEnabledResponse
	CheckEnabled(ctx context.Context, req *rpc.CheckEnabledRequest) *rpc.CheckEnabledResponse
	JobRequest(ctx context.Context, desc *JobDescription)
	Stop() error
}

type CheckEnabledResponse struct {
	RoomEnabled      *utils.Promise[bool]
	PublisherEnabled *utils.Promise[bool]
	Namespaces       *utils.Promise[[]string]
}

type JobDescription struct {
	JobType     livekit.JobType
	Room        *livekit.Room
	Participant *livekit.ParticipantInfo
	Namespaces  []string
}

func (r CheckEnabledResponse) Error() error {
	if r.RoomEnabled.Err != nil {
		return r.RoomEnabled.Err
	}
	if r.PublisherEnabled.Err != nil {
		return r.PublisherEnabled.Err
	}
	if r.Namespaces.Err != nil {
		return r.Namespaces.Err
	}
	return nil
}

type agentClient struct {
	client rpc.AgentInternalClient

	mu sync.RWMutex

	// cache response to avoid constantly checking with controllers
	// cache is invalidated with AgentRegistered updates
	enabledCache     *CheckEnabledResponse
	enabledExpiresAt time.Time

	invalidateSub psrpc.Subscription[*emptypb.Empty]
	subDone       chan struct{}
}

func NewAgentClient(bus psrpc.MessageBus) (AgentClient, error) {
	client, err := rpc.NewAgentInternalClient(bus)
	if err != nil {
		return nil, err
	}

	c := &agentClient{
		client:  client,
		subDone: make(chan struct{}),
	}

	sub, err := c.client.SubscribeWorkerRegistered(context.Background(), "") // No project ID in OSS
	if err != nil {
		return nil, err
	}

	c.invalidateSub = sub

	go func() {
		// invalidate cache
		for range sub.Channel() {
			logger.Debugw("invalidating enabled agent cache")
			c.mu.Lock()
			c.enabledCache = nil
			c.mu.Unlock()
		}

		c.subDone <- struct{}{}
	}()

	return c, nil
}

func (c *agentClient) CheckEnabled(ctx context.Context, req *rpc.CheckEnabledRequest) *rpc.CheckEnabledResponse {
	res := c.CheckEnabledPromise(ctx, req)
	<-res.RoomEnabled.Done()
	<-res.PublisherEnabled.Done()
	<-res.Namespaces.Done()
	return &rpc.CheckEnabledResponse{
		RoomEnabled:      res.RoomEnabled.Result,
		PublisherEnabled: res.PublisherEnabled.Result,
		Namespaces:       res.Namespaces.Result,
	}
}

func (c *agentClient) CheckEnabledPromise(ctx context.Context, req *rpc.CheckEnabledRequest) CheckEnabledResponse {
	c.mu.RLock()
	if c.enabledCache != nil && time.Now().Before(c.enabledExpiresAt) {
		defer c.mu.RUnlock()
		return *c.enabledCache
	}
	c.mu.RUnlock()

	res := c.checkEnabled(ctx, req)
	go func() {
		<-res.RoomEnabled.Done()
		<-res.PublisherEnabled.Done()
		<-res.Namespaces.Done()
		if res.Error() == nil {
			c.mu.Lock()
			c.enabledCache = &res
			c.enabledExpiresAt = time.Now().Add(EnabledCacheTTL)
			c.mu.Unlock()
		}
	}()
	return res
}

func (c *agentClient) checkEnabled(ctx context.Context, req *rpc.CheckEnabledRequest) CheckEnabledResponse {
	resChan, err := c.client.CheckEnabled(ctx, req, psrpc.WithRequestTimeout(CheckEnabledTimeout))
	if err != nil {
		return CheckEnabledResponse{
			RoomEnabled:      resolvedFalsePromise,
			PublisherEnabled: resolvedFalsePromise,
			Namespaces:       resolvedEmptyPromise,
		}
	}

	roomEnabled := utils.NewPromise[bool]()
	publisherEnabled := utils.NewPromise[bool]()
	namespacesPromise := utils.NewPromise[[]string]() // Ignore empty namespaces

	go func() {
		nsMap := make(map[string]bool)

		for r := range resChan {
			if r.Result.GetRoomEnabled() && !roomEnabled.Resolved() {
				roomEnabled.Resolve(true, nil)
			}
			if r.Result.GetPublisherEnabled() && !publisherEnabled.Resolved() {
				publisherEnabled.Resolve(true, nil)
			}
			if r.Result.GetNamespaces() != nil {
				for _, ns := range r.Result.GetNamespaces() {
					nsMap[ns] = true
				}
			}
		}
		if !roomEnabled.Resolved() {
			roomEnabled.Resolve(false, nil)
		}
		if !publisherEnabled.Resolved() {
			publisherEnabled.Resolve(false, nil)
		}

		// Need to wait all answers before resolving namespaces
		namespaces := make([]string, 0, len(nsMap))
		for ns := range nsMap {
			namespaces = append(namespaces, ns)
		}
		namespacesPromise.Resolve(namespaces, nil)
	}()

	return CheckEnabledResponse{
		RoomEnabled:      roomEnabled,
		PublisherEnabled: publisherEnabled,
		Namespaces:       namespacesPromise,
	}
}

func (c *agentClient) JobRequest(ctx context.Context, desc *JobDescription) {
	// Send a job request for every namespace (inside the description)
	wg := sync.WaitGroup{}
	wg.Add(len(desc.Namespaces))
	for _, ns := range desc.Namespaces {
		var jobTypeTopic string
		switch desc.JobType {
		case livekit.JobType_JT_ROOM:
			jobTypeTopic = RoomAgentTopic
		case livekit.JobType_JT_PUBLISHER:
			jobTypeTopic = PublisherAgentTopic
		}

		go func(ns string, topic string) {
			defer wg.Done()
			_, err := c.client.JobRequest(ctx, ns, jobTypeTopic, &livekit.Job{
				Id:          utils.NewGuid(utils.AgentJobPrefix),
				Type:        desc.JobType,
				Room:        desc.Room,
				Participant: desc.Participant,
				Namespace:   ns,
			})
			if err != nil {
				logger.Errorw("failed to send job request", err, "namespace", ns, "jobType", desc.JobType)
			}
		}(ns, jobTypeTopic)

	}
	wg.Wait()
}

func (c *agentClient) Stop() error {
	_ = c.invalidateSub.Close()
	<-c.subDone
	return nil
}
