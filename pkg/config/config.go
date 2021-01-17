package config

import (
	"github.com/urfave/cli/v2"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Port  uint32      `yaml:"port"`
	RTC   RTCConfig   `yaml:"rtc"`
	Redis RedisConfig `yaml:"redis"`

	// multi-node configuration,
	MultiNode   bool `yaml:"multi_node"`
	Development bool `yaml:"development"`
}

type RTCConfig struct {
	ICEPortRangeStart uint16   `yaml:"port_range_start"`
	ICEPortRangeEnd   uint16   `yaml:"port_range_end"`
	StunServers       []string `yaml:"stun_servers"`
	UseExternalIP     bool     `yaml:"use_external_ip"`

	MaxBitrate    uint64 `yaml:"max_bandwidth"`
	MaxBufferTime int    `yaml:"max_buffer_time"`
}

type RedisConfig struct {
	Address  string `yaml:"address"`
	Password string `yaml:"password"`
}

func NewConfig(confString string) (*Config, error) {
	// start with defaults
	conf := &Config{
		Port: 7880,
		RTC: RTCConfig{
			ICEPortRangeStart: 8000,
			ICEPortRangeEnd:   10000,
			StunServers: []string{
				"stun.l.google.com:19302",
			},
		},
		Redis: RedisConfig{
			Address: "localhost:6379",
		},
	}
	if confString != "" {
		yaml.Unmarshal([]byte(confString), conf)
	}
	return conf, nil
}

func (conf *Config) UpdateFromCLI(c *cli.Context) {
	if c.IsSet("dev") {
		conf.Development = c.Bool("dev")
	}
}
