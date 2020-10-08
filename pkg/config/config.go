package config

import (
	"github.com/urfave/cli/v2"
	"gopkg.in/yaml.v2"
)

type Config struct {
	APIPort       uint32 `yaml:"api_port"`
	RTCPort       uint32 `yaml:"rtc_port"`
	UDPRangeStart uint32 `yaml:"udp_range_start"`
	UDPRangeEnd   uint32 `yaml:"udp_range_end"`

	// multi-node configuration,
	MultiNode   bool `yaml:"multi_node"`
	Development bool `yaml:"development"`

	// Stun server
	StunServer string `yaml:"stun_server"`
}

func NewConfig(confString string) (*Config, error) {
	// start with defaults
	conf := &Config{
		APIPort:       7880,
		RTCPort:       7881,
		UDPRangeStart: 10000,
		UDPRangeEnd:   11000,
		StunServer:    "stun.l.google.com:19302",
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
