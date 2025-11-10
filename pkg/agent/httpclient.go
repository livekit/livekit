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
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

const (
	DefaultHTTPTimeout = 10 * time.Second
	SignatureHeader    = "X-LiveKit-Signature"
	TimestampHeader    = "X-LiveKit-Timestamp"
)

// JobNotification represents the payload sent to external agent webhooks
type JobNotification struct {
	JobID       string                    `json:"job_id"`
	DispatchID  string                    `json:"dispatch_id"`
	JobType     string                    `json:"job_type"`
	Room        *livekit.Room             `json:"room"`
	Participant *livekit.ParticipantInfo  `json:"participant,omitempty"`
	Metadata    string                    `json:"metadata,omitempty"`
	AccessToken string                    `json:"access_token"`
	ServerURL   string                    `json:"server_url"`
	Timestamp   int64                     `json:"timestamp"`
}

// JobNotificationResponse represents the expected response from external agents
type JobNotificationResponse struct {
	Accepted              bool              `json:"accepted"`
	ParticipantIdentity   string            `json:"participant_identity,omitempty"`
	ParticipantName       string            `json:"participant_name,omitempty"`
	ParticipantMetadata   string            `json:"participant_metadata,omitempty"`
	ParticipantAttributes map[string]string `json:"participant_attributes,omitempty"`
	Error                 string            `json:"error,omitempty"`
}

// HTTPAgentClient handles HTTP communication with external agents
type HTTPAgentClient struct {
	config     ExternalAgent
	httpClient *http.Client
	logger     logger.Logger
}

// NewHTTPAgentClient creates a new HTTP agent client
func NewHTTPAgentClient(config ExternalAgent, log logger.Logger) *HTTPAgentClient {
	timeout := config.Timeout
	if timeout == 0 {
		timeout = DefaultHTTPTimeout
	}

	return &HTTPAgentClient{
		config: config,
		httpClient: &http.Client{
			Timeout: timeout,
		},
		logger: log.WithValues("externalAgent", config.Name),
	}
}

// SendJobNotification sends a job notification to the external agent webhook
func (c *HTTPAgentClient) SendJobNotification(
	ctx context.Context,
	notification *JobNotification,
) (*JobNotificationResponse, error) {
	// Marshal payload
	payload, err := json.Marshal(notification)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal job notification: %w", err)
	}

	// Create request
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.WebhookURL, bytes.NewBuffer(payload))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(TimestampHeader, fmt.Sprintf("%d", notification.Timestamp))

	// Add signature
	signature := c.generateSignature(payload)
	req.Header.Set(SignatureHeader, signature)

	// Add custom headers
	for key, value := range c.config.Headers {
		req.Header.Set(key, value)
	}

	// Send request with retry logic
	var resp *http.Response
	var lastErr error

	maxRetries := c.config.Retry.MaxRetries
	if maxRetries == 0 {
		maxRetries = 3 // Default
	}

	initialWait := c.config.Retry.InitialWait
	if initialWait == 0 {
		initialWait = 1 * time.Second
	}

	maxWait := c.config.Retry.MaxWait
	if maxWait == 0 {
		maxWait = 10 * time.Second
	}

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff
			wait := initialWait * time.Duration(1<<uint(attempt-1))
			if wait > maxWait {
				wait = maxWait
			}

			c.logger.Debugw("retrying webhook request",
				"attempt", attempt,
				"wait", wait,
				"jobID", notification.JobID)

			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		resp, lastErr = c.httpClient.Do(req)
		if lastErr == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			break
		}

		if resp != nil {
			resp.Body.Close()
		}
	}

	if lastErr != nil {
		return nil, fmt.Errorf("failed after %d retries: %w", maxRetries, lastErr)
	}

	defer resp.Body.Close()

	// Check status code
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("webhook returned status %d: %s", resp.StatusCode, string(body))
	}

	// Parse response
	var notificationResp JobNotificationResponse
	if err := json.NewDecoder(resp.Body).Decode(&notificationResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	if !notificationResp.Accepted {
		return &notificationResp, fmt.Errorf("agent rejected job: %s", notificationResp.Error)
	}

	return &notificationResp, nil
}

// generateSignature creates an HMAC-SHA256 signature for the payload
func (c *HTTPAgentClient) generateSignature(payload []byte) string {
	h := hmac.New(sha256.New, []byte(c.config.Secret))
	h.Write(payload)
	return hex.EncodeToString(h.Sum(nil))
}

// ValidateSignature validates the signature from a webhook callback
func (c *HTTPAgentClient) ValidateSignature(payload []byte, signature string) bool {
	expected := c.generateSignature(payload)
	return hmac.Equal([]byte(expected), []byte(signature))
}

// GetName returns the agent name
func (c *HTTPAgentClient) GetName() string {
	return c.config.Name
}

// SupportsJobType checks if the agent supports a specific job type
func (c *HTTPAgentClient) SupportsJobType(jobType livekit.JobType) bool {
	jobTypeStr := jobType.String()
	for _, supported := range c.config.JobTypes {
		if supported == jobTypeStr {
			return true
		}
	}
	return false
}
