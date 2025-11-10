package agent

import "time"

type Config struct {
	EnableUserDataRecording bool            `yaml:"enable_user_data_recording"`
	ExternalAgents          []ExternalAgent `yaml:"external_agents,omitempty"`
}

// ExternalAgent represents an HTTP-based external agent configuration
// that allows integration with AI SDK, TypeScript, or any HTTP-capable service
type ExternalAgent struct {
	// Name is the unique identifier for this external agent
	Name string `yaml:"name"`

	// WebhookURL is the HTTP endpoint where job notifications will be sent
	WebhookURL string `yaml:"webhook_url"`

	// JobTypes specifies which types of jobs this agent can handle
	// Options: JT_ROOM, JT_PUBLISHER, JT_PARTICIPANT
	JobTypes []string `yaml:"job_types"`

	// Secret is used for HMAC signature validation of webhook payloads
	Secret string `yaml:"secret"`

	// Timeout for HTTP requests to the webhook URL
	Timeout time.Duration `yaml:"timeout,omitempty"`

	// Retry configuration for failed webhook deliveries
	Retry RetryConfig `yaml:"retry,omitempty"`

	// Headers to include in webhook requests
	Headers map[string]string `yaml:"headers,omitempty"`

	// Enabled controls whether this external agent is active
	Enabled bool `yaml:"enabled"`
}

// RetryConfig defines retry behavior for webhook delivery failures
type RetryConfig struct {
	// MaxRetries is the maximum number of retry attempts
	MaxRetries int `yaml:"max_retries,omitempty"`

	// InitialWait is the initial wait time before first retry
	InitialWait time.Duration `yaml:"initial_wait,omitempty"`

	// MaxWait is the maximum wait time between retries
	MaxWait time.Duration `yaml:"max_wait,omitempty"`
}
