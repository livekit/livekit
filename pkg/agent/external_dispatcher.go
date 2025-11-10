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

	pagent "github.com/livekit/protocol/agent"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	serverutils "github.com/livekit/livekit-server/pkg/utils"
)

// ExternalDispatcher manages HTTP-based external agents
type ExternalDispatcher struct {
	clients   map[string]*HTTPAgentClient
	apiKey    string
	apiSecret string
	serverURL string
	logger    logger.Logger
	mu        sync.RWMutex
}

// NewExternalDispatcher creates a new external agent dispatcher
func NewExternalDispatcher(
	config Config,
	apiKey string,
	apiSecret string,
	serverURL string,
	log logger.Logger,
) *ExternalDispatcher {
	dispatcher := &ExternalDispatcher{
		clients:   make(map[string]*HTTPAgentClient),
		apiKey:    apiKey,
		apiSecret: apiSecret,
		serverURL: serverURL,
		logger:    log.WithComponent("external-agent-dispatcher"),
	}

	// Initialize HTTP clients for each enabled external agent
	for _, agentConfig := range config.ExternalAgents {
		if !agentConfig.Enabled {
			continue
		}

		client := NewHTTPAgentClient(agentConfig, log)
		dispatcher.clients[agentConfig.Name] = client
		dispatcher.logger.Infow("registered external agent",
			"name", agentConfig.Name,
			"webhookURL", agentConfig.WebhookURL,
			"jobTypes", agentConfig.JobTypes)
	}

	return dispatcher
}

// DispatchJob dispatches a job to matching external agents
func (d *ExternalDispatcher) DispatchJob(
	ctx context.Context,
	desc *JobRequest,
) *serverutils.IncrementalDispatcher[*livekit.Job] {
	dispatcher := serverutils.NewIncrementalDispatcher[*livekit.Job]()

	// Find matching external agents
	var matchingClients []*HTTPAgentClient
	d.mu.RLock()
	for _, client := range d.clients {
		// Check if agent name matches (if specified)
		if desc.AgentName != "" && client.GetName() != desc.AgentName {
			continue
		}

		// Check if agent supports this job type
		if !client.SupportsJobType(desc.JobType) {
			continue
		}

		matchingClients = append(matchingClients, client)
	}
	d.mu.RUnlock()

	if len(matchingClients) == 0 {
		d.logger.Debugw("no external agents match job requirements",
			"agentName", desc.AgentName,
			"jobType", desc.JobType,
			"room", desc.Room.Name)
		dispatcher.Done()
		return dispatcher
	}

	// Dispatch to all matching external agents
	var wg sync.WaitGroup
	for _, client := range matchingClients {
		wg.Add(1)
		go func(c *HTTPAgentClient) {
			defer wg.Done()

			job, err := d.dispatchToClient(ctx, c, desc)
			if err != nil {
				d.logger.Warnw("failed to dispatch job to external agent", err,
					"agent", c.GetName(),
					"jobID", desc.DispatchId)
				return
			}

			dispatcher.Add(job)
		}(client)
	}

	// Wait for all dispatches to complete
	go func() {
		wg.Wait()
		dispatcher.Done()
	}()

	return dispatcher
}

// dispatchToClient sends a job notification to a specific external agent
func (d *ExternalDispatcher) dispatchToClient(
	ctx context.Context,
	client *HTTPAgentClient,
	desc *JobRequest,
) (*livekit.Job, error) {
	job := &livekit.Job{
		Id:          desc.DispatchId,
		DispatchId:  desc.DispatchId,
		Type:        desc.JobType,
		Room:        desc.Room,
		Participant: desc.Participant,
		Namespace:   "", // External agents don't use namespaces
		AgentName:   client.GetName(),
		Metadata:    desc.Metadata,
	}

	// Initialize job state
	job.State = &livekit.JobState{
		Status: livekit.JobStatus_JS_PENDING,
	}

	// Generate access token for the agent to join the room
	token, err := d.generateAgentToken(job, client)
	if err != nil {
		return nil, fmt.Errorf("failed to generate agent token: %w", err)
	}

	// Create notification payload
	notification := &JobNotification{
		JobID:       job.Id,
		DispatchID:  job.DispatchId,
		JobType:     job.Type.String(),
		Room:        job.Room,
		Participant: job.Participant,
		Metadata:    job.Metadata,
		AccessToken: token,
		ServerURL:   d.serverURL,
		Timestamp:   now.Unix(),
	}

	// Send notification to external agent
	response, err := client.SendJobNotification(ctx, notification)
	if err != nil {
		job.State.Status = livekit.JobStatus_JS_FAILED
		job.State.Error = err.Error()
		return job, err
	}

	// Update job state with response
	job.State.Status = livekit.JobStatus_JS_RUNNING
	job.State.StartedAt = now.UnixNano()
	job.State.ParticipantIdentity = response.ParticipantIdentity
	job.State.AgentId = client.GetName()
	job.State.WorkerId = "external:" + client.GetName()

	d.logger.Infow("dispatched job to external agent",
		"jobID", job.Id,
		"agent", client.GetName(),
		"participantIdentity", response.ParticipantIdentity,
		"room", job.Room.Name)

	return job, nil
}

// generateAgentToken creates an access token for the external agent
func (d *ExternalDispatcher) generateAgentToken(
	job *livekit.Job,
	client *HTTPAgentClient,
) (string, error) {
	// Default participant identity if not provided
	participantIdentity := fmt.Sprintf("agent-%s-%s", client.GetName(), job.Id)

	// Use external agent name as participant name
	participantName := fmt.Sprintf("AI Agent (%s)", client.GetName())

	// Build attributes
	attributes := map[string]string{
		AgentNameAttributeKey: client.GetName(),
		"job_id":              job.Id,
		"job_type":            job.Type.String(),
		"agent_type":          "external",
	}

	// Generate token with default permissions
	permissions := &livekit.ParticipantPermission{
		CanSubscribe:      true,
		CanPublish:        true,
		CanPublishData:    true,
		CanUpdateMetadata: true,
	}

	token, err := pagent.BuildAgentToken(
		d.apiKey,
		d.apiSecret,
		job.Room.Name,
		participantIdentity,
		participantName,
		job.Metadata,
		attributes,
		permissions,
	)
	if err != nil {
		return "", err
	}

	return token, nil
}

// GetClient returns an HTTP client by name
func (d *ExternalDispatcher) GetClient(name string) (*HTTPAgentClient, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	client, ok := d.clients[name]
	return client, ok
}

// HasExternalAgents returns true if any external agents are configured
func (d *ExternalDispatcher) HasExternalAgents() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.clients) > 0
}
