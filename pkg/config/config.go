package config

import (
	"os"
	"time"

	"github.com/mitchellh/go-homedir"
	"github.com/urfave/cli/v2"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Port           uint32            `yaml:"port"`
	PrometheusPort uint32            `yaml:"prometheus_port"`
	RTC            RTCConfig         `yaml:"rtc"`
	Redis          RedisConfig       `yaml:"redis"`
	Audio          AudioConfig       `yaml:"audio"`
	TURN           TURNConfig        `yaml:"turn"`
	KeyFile        string            `yaml:"key_file"`
	Keys           map[string]string `yaml:"keys"`
	LogLevel       string            `yaml:"log_level"`

	Development bool `yaml:"development"`
}

type RTCConfig struct {
	UDPPort           uint32 `yaml:"udp_port"`
	TCPPort           uint32 `yaml:"tcp_port"`
	ICEPortRangeStart uint32 `yaml:"port_range_start"`
	ICEPortRangeEnd   uint32 `yaml:"port_range_end"`
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
	// audioLevel will be an average of `smooth_samples`, 0 to disable
	SmoothSamples uint32 `yaml:"smooth_samples"`
}

type RedisConfig struct {
	Address  string `yaml:"address"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`
}

type TURNConfig struct {
	Enabled        bool   `yaml:"enabled"`
	Domain         string `yaml:"domain"`
	CertFile       string `yaml:"cert_file"`
	KeyFile        string `yaml:"key_file"`
	TLSPort        int    `yaml:"tls_port"`
	PortRangeStart uint16 `yaml:"port_range_start"`
	PortRangeEnd   uint16 `yaml:"port_range_end"`
}

func NewConfig(confString string) (*Config, error) {
	// start with defaults
	conf := &Config{
		Port: 7880,
		RTC: RTCConfig{
			UseExternalIP:     false,
			TCPPort:           7881,
			UDPPort:           7882,
			ICEPortRangeStart: 0,
			ICEPortRangeEnd:   0,
			StunServers: []string{
				"stun.l.google.com:19302",
				"stun1.l.google.com:19302",
			},
			MaxBitrate:       3 * 1024 * 1024, // 3 mbps
			PacketBufferSize: 500,
			PLIThrottle: PLIThrottleConfig{
				LowQuality:  500 * time.Millisecond,
				MidQuality:  time.Second,
				HighQuality: time.Second,
			},
		},
		Audio: AudioConfig{
			ActiveLevel:    40,
			MinPercentile:  15,
			UpdateInterval: 500,
			SmoothSamples:  5,
		},
		Redis: RedisConfig{},
		TURN: TURNConfig{
			Enabled:        false,
			TLSPort:        3478,
			PortRangeStart: 12000,
			PortRangeEnd:   14000,
		},
		Keys: map[string]string{},
	}
	if confString != "" {
		yaml.Unmarshal([]byte(confString), conf)
	}
	return conf, nil
}

func (conf *Config) HasRedis() bool {
	return conf.Redis.Address != ""
}

func (conf *Config) UpdateFromCLI(c *cli.Context) error {
	if c.IsSet("dev") {
		conf.Development = c.Bool("dev")
	}
	if c.IsSet("key-file") {
		conf.KeyFile = c.String("key-file")
	}
	if c.IsSet("keys") {
		if err := conf.unmarshalKeys(c.String("keys")); err != nil {
			return err
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
	// expand env vars in filenames
	file, err := homedir.Expand(os.ExpandEnv(conf.KeyFile))
	if err != nil {
		return err
	}
	conf.KeyFile = file

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
