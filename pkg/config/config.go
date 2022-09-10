package config

import (
	"fmt"
	"os"
	"time"

	"github.com/mitchellh/go-homedir"
	"github.com/pion/webrtc/v3"
	"github.com/pkg/errors"
	"github.com/urfave/cli/v2"
	"gopkg.in/yaml.v3"

	"github.com/livekit/protocol/logger"
)

var DefaultStunServers = []string{
	"stun.l.google.com:19302",
	"stun1.l.google.com:19302",
}

type CongestionControlProbeMode string

const (
	CongestionControlProbeModePadding CongestionControlProbeMode = "padding"
	CongestionControlProbeModeMedia   CongestionControlProbeMode = "media"

	StatsUpdateInterval = time.Second * 10
)

type Config struct {
	Port           uint32             `yaml:"port"`
	BindAddresses  []string           `yaml:"bind_addresses"`
	PrometheusPort uint32             `yaml:"prometheus_port,omitempty"`
	RTC            RTCConfig          `yaml:"rtc,omitempty"`
	Redis          RedisConfig        `yaml:"redis,omitempty"`
	Audio          AudioConfig        `yaml:"audio,omitempty"`
	Video          VideoConfig        `yaml:"video,omitempty"`
	Room           RoomConfig         `yaml:"room,omitempty"`
	TURN           TURNConfig         `yaml:"turn,omitempty"`
	Ingress        IngressConfig      `yaml:"ingress,omitempty"`
	WebHook        WebHookConfig      `yaml:"webhook,omitempty"`
	NodeSelector   NodeSelectorConfig `yaml:"node_selector,omitempty"`
	KeyFile        string             `yaml:"key_file,omitempty"`
	Keys           map[string]string  `yaml:"keys,omitempty"`
	Region         string             `yaml:"region,omitempty"`
	// LogLevel is deprecated
	LogLevel string        `yaml:"log_level,omitempty"`
	Logging  LoggingConfig `yaml:"logging,omitempty"`
	Limit    LimitConfig   `yaml:"limit,omitempty"`

	Development bool `yaml:"development,omitempty"`
}

type RTCConfig struct {
	UDPPort           uint32           `yaml:"udp_port,omitempty"`
	TCPPort           uint32           `yaml:"tcp_port,omitempty"`
	ICEPortRangeStart uint32           `yaml:"port_range_start,omitempty"`
	ICEPortRangeEnd   uint32           `yaml:"port_range_end,omitempty"`
	NodeIP            string           `yaml:"node_ip,omitempty"`
	STUNServers       []string         `yaml:"stun_servers,omitempty"`
	TURNServers       []TURNServer     `yaml:"turn_servers,omitempty"`
	UseExternalIP     bool             `yaml:"use_external_ip"`
	UseICELite        bool             `yaml:"use_ice_lite,omitempty"`
	Interfaces        InterfacesConfig `yaml:"interfaces"`

	// Number of packets to buffer for NACK
	PacketBufferSize int `yaml:"packet_buffer_size,omitempty"`

	// Throttle periods for pli/fir rtcp packets
	PLIThrottle PLIThrottleConfig `yaml:"pli_throttle,omitempty"`

	CongestionControl CongestionControlConfig `yaml:"congestion_control,omitempty"`

	// allow TCP and TURN/TLS fallback
	AllowTCPFallback *bool `yaml:"allow_tcp_fallback,omitempty"`

	// for testing, disable UDP
	ForceTCP bool `yaml:"force_tcp,omitempty"`
}

type TURNServer struct {
	Host       string `yaml:"host"`
	Port       int    `yaml:"port"`
	Protocol   string `yaml:"protocol"`
	Username   string `yaml:"username,omitempty"`
	Credential string `yaml:"credential,omitempty"`
}

type PLIThrottleConfig struct {
	LowQuality  time.Duration `yaml:"low_quality,omitempty"`
	MidQuality  time.Duration `yaml:"mid_quality,omitempty"`
	HighQuality time.Duration `yaml:"high_quality,omitempty"`
}

type CongestionControlConfig struct {
	Enabled            bool                       `yaml:"enabled"`
	AllowPause         bool                       `yaml:"allow_pause"`
	UseSendSideBWE     bool                       `yaml:"send_side_bandwidth_estimation,omitempty"`
	ProbeMode          CongestionControlProbeMode `yaml:"padding_mode,omitempty"`
	MinChannelCapacity int64                      `yaml:"min_channel_capacity,omitempty"`
}

type InterfacesConfig struct {
	Includes []string `yaml:"includes"`
	Excludes []string `yaml:"excludes"`
}

type AudioConfig struct {
	// minimum level to be considered active, 0-127, where 0 is loudest
	ActiveLevel uint8 `yaml:"active_level"`
	// percentile to measure, a participant is considered active if it has exceeded the ActiveLevel more than
	// MinPercentile% of the time
	MinPercentile uint8 `yaml:"min_percentile"`
	// interval to update clients, in ms
	UpdateInterval uint32 `yaml:"update_interval"`
	// smoothing for audioLevel values sent to the client.
	// audioLevel will be an average of `smooth_intervals`, 0 to disable
	SmoothIntervals uint32 `yaml:"smooth_intervals"`
}

type VideoConfig struct {
	DynacastPauseDelay time.Duration `yaml:"dynacast_pause_delay,omitempty"`
}

type RedisConfig struct {
	Address           string   `yaml:"address"`
	Username          string   `yaml:"username"`
	Password          string   `yaml:"password"`
	DB                int      `yaml:"db"`
	UseTLS            bool     `yaml:"use_tls"`
	MasterName        string   `yaml:"sentinel_master_name"`
	SentinelUsername  string   `yaml:"sentinel_username"`
	SentinelPassword  string   `yaml:"sentinel_password"`
	SentinelAddresses []string `yaml:"sentinel_addresses"`
}

type RoomConfig struct {
	// enable rooms to be automatically created
	AutoCreate         bool        `yaml:"auto_create"`
	EnabledCodecs      []CodecSpec `yaml:"enabled_codecs"`
	MaxParticipants    uint32      `yaml:"max_participants"`
	EmptyTimeout       uint32      `yaml:"empty_timeout"`
	EnableRemoteUnmute bool        `yaml:"enable_remote_unmute"`
	MaxMetadataSize    uint32      `yaml:"max_metadata_size"`
}

type CodecSpec struct {
	Mime     string `yaml:"mime"`
	FmtpLine string `yaml:"fmtp_line"`
}

type LoggingConfig struct {
	logger.Config `yaml:",inline"`
	PionLevel     string `yaml:"pion_level,omitempty"`
}

type TURNConfig struct {
	Enabled             bool   `yaml:"enabled"`
	Domain              string `yaml:"domain"`
	CertFile            string `yaml:"cert_file"`
	KeyFile             string `yaml:"key_file"`
	TLSPort             int    `yaml:"tls_port"`
	UDPPort             int    `yaml:"udp_port"`
	RelayPortRangeStart uint16 `yaml:"relay_range_start,omitempty"`
	RelayPortRangeEnd   uint16 `yaml:"relay_range_end,omitempty"`
	ExternalTLS         bool   `yaml:"external_tls"`
}

type WebHookConfig struct {
	URLs []string `yaml:"urls"`
	// key to use for webhook
	APIKey string `yaml:"api_key"`
}

type NodeSelectorConfig struct {
	Kind         string         `yaml:"kind"`
	SortBy       string         `yaml:"sort_by"`
	CPULoadLimit float32        `yaml:"cpu_load_limit"`
	SysloadLimit float32        `yaml:"sysload_limit"`
	Regions      []RegionConfig `yaml:"regions"`
}

// RegionConfig lists available regions and their latitude/longitude, so the selector would prefer
// regions that are closer
type RegionConfig struct {
	Name string  `yaml:"name"`
	Lat  float64 `yaml:"lat"`
	Lon  float64 `yaml:"lon"`
}

type LimitConfig struct {
	NumTracks   int32   `yaml:"num_tracks"`
	BytesPerSec float32 `yaml:"bytes_per_sec"`
}

type IngressConfig struct {
	RTMPBaseURL string `yaml:"rtmp_base_url"`
}

func NewConfig(confString string, c *cli.Context) (*Config, error) {
	// start with defaults
	conf := &Config{
		Port: 7880,
		RTC: RTCConfig{
			UseExternalIP:     false,
			TCPPort:           7881,
			UDPPort:           0,
			ICEPortRangeStart: 0,
			ICEPortRangeEnd:   0,
			STUNServers:       []string{},
			PacketBufferSize:  500,
			PLIThrottle: PLIThrottleConfig{
				LowQuality:  500 * time.Millisecond,
				MidQuality:  time.Second,
				HighQuality: time.Second,
			},
			CongestionControl: CongestionControlConfig{
				Enabled:    true,
				AllowPause: false,
				ProbeMode:  CongestionControlProbeModePadding,
			},
		},
		Audio: AudioConfig{
			ActiveLevel:     35, // -35dBov
			MinPercentile:   40,
			UpdateInterval:  400,
			SmoothIntervals: 2,
		},
		Video: VideoConfig{
			DynacastPauseDelay: 5 * time.Second,
		},
		Redis: RedisConfig{},
		Room: RoomConfig{
			AutoCreate: true,
			EnabledCodecs: []CodecSpec{
				{Mime: webrtc.MimeTypeOpus},
				{Mime: "audio/red"},
				{Mime: webrtc.MimeTypeVP8},
				{Mime: webrtc.MimeTypeH264},
				// {Mime: webrtc.MimeTypeAV1},
				// {Mime: webrtc.MimeTypeVP9},
			},
			EmptyTimeout: 5 * 60,
		},
		Logging: LoggingConfig{
			PionLevel: "error",
		},
		TURN: TURNConfig{
			Enabled: false,
		},
		NodeSelector: NodeSelectorConfig{
			Kind:         "any",
			SortBy:       "random",
			SysloadLimit: 0.9,
			CPULoadLimit: 0.9,
		},
		Keys: map[string]string{},
	}
	if confString != "" {
		if err := yaml.Unmarshal([]byte(confString), conf); err != nil {
			return nil, fmt.Errorf("could not parse config: %v", err)
		}
	}

	if c != nil {
		if err := conf.updateFromCLI(c); err != nil {
			return nil, err
		}
	}

	// expand env vars in filenames
	file, err := homedir.Expand(os.ExpandEnv(conf.KeyFile))
	if err != nil {
		return nil, err
	}
	conf.KeyFile = file

	// set defaults for ports if none are set
	if conf.RTC.UDPPort == 0 && conf.RTC.ICEPortRangeStart == 0 {
		// to make it easier to run in dev mode/docker, default to single port
		if conf.Development {
			conf.RTC.UDPPort = 7882
		} else {
			conf.RTC.ICEPortRangeStart = 50000
			conf.RTC.ICEPortRangeEnd = 60000
		}
	}

	// set defaults for Turn relay if none are set
	if conf.TURN.RelayPortRangeStart == 0 || conf.TURN.RelayPortRangeEnd == 0 {
		// to make it easier to run in dev mode/docker, default to two ports
		if conf.Development {
			conf.TURN.RelayPortRangeStart = 30000
			conf.TURN.RelayPortRangeEnd = 30002
		} else {
			conf.TURN.RelayPortRangeStart = 30000
			conf.TURN.RelayPortRangeEnd = 40000
		}
	}

	if conf.RTC.NodeIP == "" {
		conf.RTC.NodeIP, err = conf.determineIP()
		if err != nil {
			return nil, err
		}
	}

	if conf.LogLevel != "" {
		conf.Logging.Level = conf.LogLevel
	}
	if conf.Logging.Level == "" && conf.Development {
		conf.Logging.Level = "debug"
	}

	return conf, nil
}

func (conf *Config) HasRedis() bool {
	return conf.Redis.Address != "" || conf.Redis.SentinelAddresses != nil
}

func (conf *Config) UseSentinel() bool {
	return conf.Redis.SentinelAddresses != nil
}

func (conf *Config) IsTURNSEnabled() bool {
	if conf.TURN.Enabled && conf.TURN.TLSPort != 0 {
		return true
	}
	for _, s := range conf.RTC.TURNServers {
		if s.Protocol == "tls" {
			return true
		}
	}
	return false
}

func (conf *Config) updateFromCLI(c *cli.Context) error {
	if c.IsSet("dev") {
		conf.Development = c.Bool("dev")
	}
	if c.IsSet("key-file") {
		conf.KeyFile = c.String("key-file")
	}
	if c.IsSet("keys") {
		if err := conf.unmarshalKeys(c.String("keys")); err != nil {
			return errors.New("Could not parse keys, it needs to be exactly, \"key: secret\", including the space")
		}
	}
	if c.IsSet("region") {
		conf.Region = c.String("region")
	}
	if c.IsSet("redis-host") {
		conf.Redis.Address = c.String("redis-host")
	}
	if c.IsSet("redis-password") {
		conf.Redis.Password = c.String("redis-password")
	}
	if c.IsSet("turn-cert") {
		conf.TURN.CertFile = c.String("turn-cert")
	}
	if c.IsSet("turn-key") {
		conf.TURN.KeyFile = c.String("turn-key")
	}
	if c.IsSet("node-ip") {
		conf.RTC.NodeIP = c.String("node-ip")
	}
	if c.IsSet("udp-port") {
		conf.RTC.UDPPort = uint32(c.Int("udp-port"))
	}
	if c.IsSet("bind") {
		conf.BindAddresses = c.StringSlice("bind")
	}
	return nil
}

func (conf *Config) unmarshalKeys(keys string) error {
	temp := make(map[string]interface{})
	if err := yaml.Unmarshal([]byte(keys), temp); err != nil {
		return err
	}

	conf.Keys = make(map[string]string, len(temp))

	for key, val := range temp {
		if secret, ok := val.(string); ok {
			conf.Keys[key] = secret
		}
	}
	return nil
}
