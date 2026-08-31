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

	// WebTransportPort is the UDP port for the worker WebTransport listener that
	// carries the control stream and HTTP exchanges. 0 disables the listener, so
	// no worker can serve endpoints (the front then only relays, in cloud).
	WebTransportPort uint32 `yaml:"webtransport_port,omitempty"`
	// TLSCertFile / TLSKeyFile is the certificate the WebTransport (QUIC) listener
	// presents. QUIC has no plaintext mode, so a listener needs one; in dev mode a
	// self-signed cert is generated when these are unset.
	TLSCertFile string `yaml:"tls_cert_file,omitempty"`
	TLSKeyFile  string `yaml:"tls_key_file,omitempty"`
	// MaxStreams is the soft per-session concurrent-stream cap used for capacity
	// weighting; 0 takes the endpoint package default.
	MaxStreams uint32 `yaml:"max_streams,omitempty"`
}
