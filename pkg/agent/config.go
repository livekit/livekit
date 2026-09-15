package agent

const DefaultTargetLoad = 0.7

type Config struct {
	EnableUserDataRecording bool    `yaml:"enable_user_data_recording"`
	EnableUserDataRedaction bool    `yaml:"enable_user_data_redaction"`
	TargetLoad              float32 `yaml:"target_load,omitempty"`

	// agent HTTP endpoints data plane; zero values take the endpoint package
	// defaults
	Endpoints EndpointsConfig `yaml:"endpoints,omitempty"`
}

type EndpointsConfig struct {
	// Disabled turns off the /agents/{agent_name}/{deployment}/... front and
	// rejects registrations that declare endpoints.
	Disabled bool `yaml:"disabled,omitempty"`

	// MaxStreams is the soft per-session concurrent-stream cap used for capacity
	// weighting; 0 takes the endpoint package default.
	MaxStreams uint32 `yaml:"max_streams,omitempty"`
}
