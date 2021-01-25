package config

import (
	"os"

	"github.com/mitchellh/go-homedir"
	"github.com/urfave/cli/v2"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Port     uint32            `yaml:"port"`
	RTC      RTCConfig         `yaml:"rtc"`
	Redis    RedisConfig       `yaml:"redis"`
	KeyFile  string            `yaml:"key_file"`
	Keys     map[string]string `yaml:"keys"`
	LogLevel string            `yaml:"log_level"`

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

func (conf *Config) UpdateFromCLI(c *cli.Context) error {
	if c.IsSet("dev") {
		conf.Development = c.Bool("dev")
	}
	if c.IsSet("key-file") {
		conf.KeyFile = c.String("key-file")
	}
	if c.IsSet("redis-host") {
		conf.Redis.Address = c.String("redis-host")
	}
	if c.IsSet("redis-password") {
		conf.Redis.Password = c.String("redis-password")
	}
	// expand env vars in filenames
	file, err := homedir.Expand(os.ExpandEnv(conf.KeyFile))
	if err != nil {
		return err
	}
	conf.KeyFile = file
	return nil
}
