package config

import (
	"github.com/urfave/cli/v2"
	"gopkg.in/yaml.v2"
)

type Config struct {
	APIPort uint32    `yaml:"api_port"`
	RTCPort uint32    `yaml:"rtc_port"`
	RTC     RTCConfig `yaml:"rtc"`

	// multi-node configuration,
	MultiNode   bool `yaml:"multi_node"`
	Development bool `yaml:"development"`
}

type RTCConfig struct {
	ICEPortRangeStart uint32   `yaml:"port_range_start"`
	ICEPortRangeEnd   uint32   `yaml:"port_range_end"`
	StunServers       []string `yaml:"stun_servers"`

	MaxBandwidth uint64            `yaml:"max_bandwidth"`
	MaxNackTime  int64             `yaml:"max_nack_time"`
	Video        WebRTCVideoConfig `yaml:"video"`
}

type WebRTCVideoConfig struct {
	REMBFeedback  bool `yaml:"remb_feedback"`
	REMBCycle     int  `yaml:"remb_cycle"`
	PLICycle      int  `yaml:"pli_cycle"`
	TCCCycle      int  `yaml:"tcc_cycle"`
	MaxBufferTime int  `yaml:"max_buffer_time"`
}

func NewConfig(confString string) (*Config, error) {
	// start with defaults
	conf := &Config{
		APIPort: 7880,
		RTCPort: 7881,
		RTC: RTCConfig{
			ICEPortRangeStart: 8000,
			ICEPortRangeEnd:   10000,
			StunServers: []string{
				"stun.l.google.com:19302",
			},
			Video: WebRTCVideoConfig{
				REMBFeedback: true,
			},
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
