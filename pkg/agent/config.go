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
	// Disabled turns off the /agents/{deployment}/... front and rejects
	// registrations that declare endpoints.
	Disabled bool `yaml:"disabled,omitempty"`

	DataConnCount     uint32 `yaml:"data_conn_count,omitempty"`
	CreditWindow      uint32 `yaml:"credit_window,omitempty"`
	ConnectionWindow  uint32 `yaml:"connection_window,omitempty"`
	MaxFrameSize      uint32 `yaml:"max_frame_size,omitempty"`
	MaxStreamsPerConn uint32 `yaml:"max_streams_per_conn,omitempty"`
}
