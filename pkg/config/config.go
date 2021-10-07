package config

import (
	"fmt"
	"os"
	"time"

	"github.com/livekit/livekit-server/pkg/routing/selector"
	"github.com/mitchellh/go-homedir"
	"github.com/pion/webrtc/v3"
	"github.com/pkg/errors"
	"github.com/urfave/cli/v2"
	"gopkg.in/yaml.v3"
)

var DefaultStunServers = []string{
	"stun.l.google.com:19302",
	"stun1.l.google.com:19302",
}

type Config struct {
	Port           uint32             `yaml:"port"`
	PrometheusPort uint32             `yaml:"prometheus_port"`
	RTC            RTCConfig          `yaml:"rtc"`
	Redis          RedisConfig        `yaml:"redis"`
	Audio          AudioConfig        `yaml:"audio"`
	Room           RoomConfig         `yaml:"room"`
	TURN           TURNConfig         `yaml:"turn"`
	WebHook        WebHookConfig      `yaml:"webhook"`
	NodeSelector   NodeSelectorConfig `yaml:"node_selector"`
	KeyFile        string             `yaml:"key_file"`
	Keys           map[string]string  `yaml:"keys"`
	Region         string             `yaml:"region"`
	LogLevel       string             `yaml:"log_level"`

	Development bool `yaml:"development"`
}

type RTCConfig struct {
	UDPPort           uint32 `yaml:"udp_port"`
	TCPPort           uint32 `yaml:"tcp_port"`
	ICEPortRangeStart uint32 `yaml:"port_range_start"`
	ICEPortRangeEnd   uint32 `yaml:"port_range_end"`
	NodeIP            string `yaml:"node_ip"`
	// for testing, disable UDP
	ForceTCP      bool     `yaml:"force_tcp"`
	StunServers   []string `yaml:"stun_servers"`
	UseExternalIP bool     `yaml:"use_external_ip"`

	// Number of packets to buffer for NACK
	PacketBufferSize int `yaml:"packet_buffer_size"`

	// Max bitrate for REMB
	MaxBitrate uint64 `yaml:"max_bitrate"`

	// Throttle periods for pli/fir rtcp packets
	PLIThrottle PLIThrottleConfig `yaml:"pli_throttle"`
}

type PLIThrottleConfig struct {
	LowQuality  time.Duration `yaml:"low_quality"`
	MidQuality  time.Duration `yaml:"mid_quality"`
	HighQuality time.Duration `yaml:"high_quality"`
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

type RedisConfig struct {
	Address  string `yaml:"address"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`
}

type RoomConfig struct {
	EnabledCodecs      []CodecSpec `yaml:"enabled_codecs"`
	MaxParticipants    uint32      `yaml:"max_participants"`
	EmptyTimeout       uint32      `yaml:"empty_timeout"`
	EnableRemoteUnmute bool        `yaml:"enable_remote_unmute"`
}

type CodecSpec struct {
	Mime     string `yaml:"mime"`
	FmtpLine string `yaml:"fmtp_line"`
}

type TURNConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Domain   string `yaml:"domain"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
	TLSPort  int    `yaml:"tls_port"`
	UDPPort  int    `yaml:"udp_port"`
}

type WebHookConfig struct {
	URLs []string `yaml:"urls"`
	// key to use for webhook
	APIKey string `yaml:"api_key"`
}

type NodeSelectorConfig struct {
	Kind         string                  `yaml:"kind"`
	SysloadLimit float32                 `yaml:"sysload_limit"`
	Regions      []selector.RegionConfig `yaml:"regions"`
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
			StunServers:       []string{},
			MaxBitrate:        3 * 1024 * 1024, // 3 mbps
			PacketBufferSize:  500,
			PLIThrottle: PLIThrottleConfig{
				LowQuality:  500 * time.Millisecond,
				MidQuality:  time.Second,
				HighQuality: time.Second,
			},
		},
		Audio: AudioConfig{
			ActiveLevel:     30, // -30dBov = 0.03
			MinPercentile:   40,
			UpdateInterval:  500,
			SmoothIntervals: 4,
		},
		Redis: RedisConfig{},
		Room: RoomConfig{
			// by default only enable opus and VP8
			EnabledCodecs: []CodecSpec{
				{Mime: webrtc.MimeTypeOpus},
				{Mime: webrtc.MimeTypeVP8},
				// {Mime: webrtc.MimeTypeH264},
				// {Mime: webrtc.MimeTypeVP9},
			},
			EmptyTimeout: 5 * 60,
		},
		TURN: TURNConfig{
			Enabled: false,
		},
		NodeSelector: NodeSelectorConfig{
			Kind:         "random",
			SysloadLimit: 0.7,
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

	if conf.RTC.NodeIP == "" {
		conf.RTC.NodeIP, err = conf.determineIP()
		if err != nil {
			return nil, err
		}
	}
	return conf, nil
}

func (conf *Config) HasRedis() bool {
	return conf.Redis.Address != ""
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

func GetAudioConfig(conf *Config) AudioConfig {
	return conf.Audio
}
