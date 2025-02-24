// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package config

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/mitchellh/go-homedir"
	"github.com/pkg/errors"
	"github.com/urfave/cli/v2"
	"gopkg.in/yaml.v3"

	"github.com/livekit/livekit-server/pkg/metric"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/bwe/remotebwe"
	"github.com/livekit/livekit-server/pkg/sfu/bwe/sendsidebwe"
	"github.com/livekit/livekit-server/pkg/sfu/mime"
	"github.com/livekit/livekit-server/pkg/sfu/streamallocator"
	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	redisLiveKit "github.com/livekit/protocol/redis"
	"github.com/livekit/protocol/rpc"
)

const (
	generatedCLIFlagUsage = "generated"
)

var (
	ErrKeyFileIncorrectPermission = errors.New("key file others permissions must be set to 0")
	ErrKeysNotSet                 = errors.New("one of key-file or keys must be provided")
)

type Config struct {
	Port          uint32   `yaml:"port,omitempty"`
	BindAddresses []string `yaml:"bind_addresses,omitempty"`
	// PrometheusPort is deprecated
	PrometheusPort uint32                   `yaml:"prometheus_port,omitempty"`
	Prometheus     PrometheusConfig         `yaml:"prometheus,omitempty"`
	RTC            RTCConfig                `yaml:"rtc,omitempty"`
	Redis          redisLiveKit.RedisConfig `yaml:"redis,omitempty"`
	Audio          sfu.AudioConfig          `yaml:"audio,omitempty"`
	Video          VideoConfig              `yaml:"video,omitempty"`
	Room           RoomConfig               `yaml:"room,omitempty"`
	TURN           TURNConfig               `yaml:"turn,omitempty"`
	Ingress        IngressConfig            `yaml:"ingress,omitempty"`
	SIP            SIPConfig                `yaml:"sip,omitempty"`
	WebHook        WebHookConfig            `yaml:"webhook,omitempty"`
	NodeSelector   NodeSelectorConfig       `yaml:"node_selector,omitempty"`
	KeyFile        string                   `yaml:"key_file,omitempty"`
	Keys           map[string]string        `yaml:"keys,omitempty"`
	Region         string                   `yaml:"region,omitempty"`
	SignalRelay    SignalRelayConfig        `yaml:"signal_relay,omitempty"`
	PSRPC          rpc.PSRPCConfig          `yaml:"psrpc,omitempty"`
	// Deprecated: LogLevel is deprecated
	LogLevel string        `yaml:"log_level,omitempty"`
	Logging  LoggingConfig `yaml:"logging,omitempty"`
	Limit    LimitConfig   `yaml:"limit,omitempty"`

	Development bool `yaml:"development,omitempty"`

	Metric metric.MetricConfig `yaml:"metric,omitempty"`
}

type RTCConfig struct {
	rtcconfig.RTCConfig `yaml:",inline"`

	TURNServers []TURNServer `yaml:"turn_servers,omitempty"`

	// Deprecated
	StrictACKs bool `yaml:"strict_acks,omitempty"`

	ICEPrflxAcceptanceWait *time.Duration `yaml:"ice_prflx_acceptance_wait,omitempty"`
	ICESrflxAcceptanceWait *time.Duration `yaml:"ice_srflx_acceptance_wait,omitempty"`
	ICERelayAcceptanceWait *time.Duration `yaml:"ice_relay_acceptance_wait,omitempty"`

	// Deprecated: use PacketBufferSizeVideo and PacketBufferSizeAudio
	PacketBufferSize int `yaml:"packet_buffer_size,omitempty"`
	// Number of packets to buffer for NACK - video
	PacketBufferSizeVideo int `yaml:"packet_buffer_size_video,omitempty"`
	// Number of packets to buffer for NACK - audio
	PacketBufferSizeAudio int `yaml:"packet_buffer_size_audio,omitempty"`

	// Throttle periods for pli/fir rtcp packets
	PLIThrottle sfu.PLIThrottleConfig `yaml:"pli_throttle,omitempty"`

	CongestionControl CongestionControlConfig `yaml:"congestion_control,omitempty"`

	// allow TCP and TURN/TLS fallback
	AllowTCPFallback *bool `yaml:"allow_tcp_fallback,omitempty"`

	// force a reconnect on a publication error
	ReconnectOnPublicationError *bool `yaml:"reconnect_on_publication_error,omitempty"`

	// force a reconnect on a subscription error
	ReconnectOnSubscriptionError *bool `yaml:"reconnect_on_subscription_error,omitempty"`

	// force a reconnect on a data channel error
	ReconnectOnDataChannelError *bool `yaml:"reconnect_on_data_channel_error,omitempty"`

	// Deprecated
	DataChannelMaxBufferedAmount uint64 `yaml:"data_channel_max_buffered_amount,omitempty"`

	// Threshold of data channel writing to be considered too slow, data packet could
	// be dropped for a slow data channel to avoid blocking the room.
	DatachannelSlowThreshold int `yaml:"datachannel_slow_threshold,omitempty"`

	ForwardStats ForwardStatsConfig `yaml:"forward_stats,omitempty"`
}

type TURNServer struct {
	Host       string `yaml:"host,omitempty"`
	Port       int    `yaml:"port,omitempty"`
	Protocol   string `yaml:"protocol,omitempty"`
	Username   string `yaml:"username,omitempty"`
	Credential string `yaml:"credential,omitempty"`
}

type CongestionControlConfig struct {
	Enabled    bool `yaml:"enabled,omitempty"`
	AllowPause bool `yaml:"allow_pause,omitempty"`

	StreamAllocator streamallocator.StreamAllocatorConfig `yaml:"stream_allocator,omitempty"`

	RemoteBWE remotebwe.RemoteBWEConfig `yaml:"remote_bwe,omitempty"`

	UseSendSideBWEInterceptor bool `yaml:"use_send_side_bwe_interceptor,omitempty"`

	UseSendSideBWE bool                          `yaml:"use_send_side_bwe,omitempty"`
	SendSideBWE    sendsidebwe.SendSideBWEConfig `yaml:"send_side_bwe,omitempty"`
}

type PlayoutDelayConfig struct {
	Enabled bool `yaml:"enabled,omitempty"`
	Min     int  `yaml:"min,omitempty"`
	Max     int  `yaml:"max,omitempty"`
}

type VideoConfig struct {
	DynacastPauseDelay   time.Duration                  `yaml:"dynacast_pause_delay,omitempty"`
	StreamTrackerManager sfu.StreamTrackerManagerConfig `yaml:"stream_tracker_manager,omitempty"`
}

type RoomConfig struct {
	// enable rooms to be automatically created
	AutoCreate         bool               `yaml:"auto_create,omitempty"`
	EnabledCodecs      []CodecSpec        `yaml:"enabled_codecs,omitempty"`
	MaxParticipants    uint32             `yaml:"max_participants,omitempty"`
	EmptyTimeout       uint32             `yaml:"empty_timeout,omitempty"`
	DepartureTimeout   uint32             `yaml:"departure_timeout,omitempty"`
	EnableRemoteUnmute bool               `yaml:"enable_remote_unmute,omitempty"`
	PlayoutDelay       PlayoutDelayConfig `yaml:"playout_delay,omitempty"`
	SyncStreams        bool               `yaml:"sync_streams,omitempty"`
	CreateRoomEnabled  bool               `yaml:"create_room_enabled,omitempty"`
	CreateRoomTimeout  time.Duration      `yaml:"create_room_timeout,omitempty"`
	CreateRoomAttempts int                `yaml:"create_room_attempts,omitempty"`
	// deprecated, moved to limits
	MaxMetadataSize uint32 `yaml:"max_metadata_size,omitempty"`
	// deprecated, moved to limits
	MaxRoomNameLength int `yaml:"max_room_name_length,omitempty"`
	// deprecated, moved to limits
	MaxParticipantIdentityLength int                                   `yaml:"max_participant_identity_length,omitempty"`
	RoomConfigurations           map[string]*livekit.RoomConfiguration `yaml:"room_configurations,omitempty"`
}

type CodecSpec struct {
	Mime     string `yaml:"mime,omitempty"`
	FmtpLine string `yaml:"fmtp_line,omitempty"`
}

type LoggingConfig struct {
	logger.Config `yaml:",inline"`
	PionLevel     string `yaml:"pion_level,omitempty"`
}

type TURNConfig struct {
	Enabled             bool   `yaml:"enabled,omitempty"`
	Domain              string `yaml:"domain,omitempty"`
	CertFile            string `yaml:"cert_file,omitempty"`
	KeyFile             string `yaml:"key_file,omitempty"`
	TLSPort             int    `yaml:"tls_port,omitempty"`
	UDPPort             int    `yaml:"udp_port,omitempty"`
	RelayPortRangeStart uint16 `yaml:"relay_range_start,omitempty"`
	RelayPortRangeEnd   uint16 `yaml:"relay_range_end,omitempty"`
	ExternalTLS         bool   `yaml:"external_tls,omitempty"`
}

type WebHookConfig struct {
	URLs []string `yaml:"urls,omitempty"`
	// key to use for webhook
	APIKey string `yaml:"api_key,omitempty"`
}

type NodeSelectorConfig struct {
	Kind         string         `yaml:"kind,omitempty"`
	SortBy       string         `yaml:"sort_by,omitempty"`
	CPULoadLimit float32        `yaml:"cpu_load_limit,omitempty"`
	SysloadLimit float32        `yaml:"sysload_limit,omitempty"`
	Regions      []RegionConfig `yaml:"regions,omitempty"`
}

type SignalRelayConfig struct {
	RetryTimeout     time.Duration `yaml:"retry_timeout,omitempty"`
	MinRetryInterval time.Duration `yaml:"min_retry_interval,omitempty"`
	MaxRetryInterval time.Duration `yaml:"max_retry_interval,omitempty"`
	StreamBufferSize int           `yaml:"stream_buffer_size,omitempty"`
	ConnectAttempts  int           `yaml:"connect_attempts,omitempty"`
}

// RegionConfig lists available regions and their latitude/longitude, so the selector would prefer
// regions that are closer
type RegionConfig struct {
	Name string  `yaml:"name,omitempty"`
	Lat  float64 `yaml:"lat,omitempty"`
	Lon  float64 `yaml:"lon,omitempty"`
}

type LimitConfig struct {
	NumTracks              int32   `yaml:"num_tracks,omitempty"`
	BytesPerSec            float32 `yaml:"bytes_per_sec,omitempty"`
	SubscriptionLimitVideo int32   `yaml:"subscription_limit_video,omitempty"`
	SubscriptionLimitAudio int32   `yaml:"subscription_limit_audio,omitempty"`
	MaxMetadataSize        uint32  `yaml:"max_metadata_size,omitempty"`
	// total size of all attributes on a participant
	MaxAttributesSize            uint32 `yaml:"max_attributes_size,omitempty"`
	MaxRoomNameLength            int    `yaml:"max_room_name_length,omitempty"`
	MaxParticipantIdentityLength int    `yaml:"max_participant_identity_length,omitempty"`
	MaxParticipantNameLength     int    `yaml:"max_participant_name_length,omitempty"`
}

func (l LimitConfig) CheckRoomNameLength(name string) bool {
	return l.MaxRoomNameLength == 0 || len(name) <= l.MaxRoomNameLength
}

func (l LimitConfig) CheckParticipantNameLength(name string) bool {
	return l.MaxParticipantNameLength == 0 || len(name) <= l.MaxParticipantNameLength
}

func (l LimitConfig) CheckMetadataSize(metadata string) bool {
	return l.MaxMetadataSize == 0 || uint32(len(metadata)) <= l.MaxMetadataSize
}

func (l LimitConfig) CheckAttributesSize(attributes map[string]string) bool {
	if l.MaxAttributesSize == 0 {
		return true
	}

	total := 0
	for k, v := range attributes {
		total += len(k) + len(v)
	}
	return uint32(total) <= l.MaxAttributesSize
}

type IngressConfig struct {
	RTMPBaseURL string `yaml:"rtmp_base_url,omitempty"`
	WHIPBaseURL string `yaml:"whip_base_url,omitempty"`
}

type SIPConfig struct{}

type APIConfig struct {
	// amount of time to wait for API to execute, default 2s
	ExecutionTimeout time.Duration `yaml:"execution_timeout,omitempty"`

	// min amount of time to wait before checking for operation complete
	CheckInterval time.Duration `yaml:"check_interval,omitempty"`

	// max amount of time to wait before checking for operation complete
	MaxCheckInterval time.Duration `yaml:"max_check_interval,omitempty"`
}

type PrometheusConfig struct {
	Port     uint32 `yaml:"port,omitempty"`
	Username string `yaml:"username,omitempty"`
	Password string `yaml:"password,omitempty"`
}

type ForwardStatsConfig struct {
	SummaryInterval time.Duration `yaml:"summary_interval,omitempty"`
	ReportInterval  time.Duration `yaml:"report_interval,omitempty"`
	ReportWindow    time.Duration `yaml:"report_window,omitempty"`
}

func DefaultAPIConfig() APIConfig {
	return APIConfig{
		ExecutionTimeout: 2 * time.Second,
		CheckInterval:    100 * time.Millisecond,
		MaxCheckInterval: 300 * time.Second,
	}
}

var DefaultConfig = Config{
	Port: 7880,
	RTC: RTCConfig{
		RTCConfig: rtcconfig.RTCConfig{
			UseExternalIP:     false,
			TCPPort:           7881,
			ICEPortRangeStart: 0,
			ICEPortRangeEnd:   0,
			STUNServers:       []string{},
		},
		PacketBufferSize:      500,
		PacketBufferSizeVideo: 500,
		PacketBufferSizeAudio: 200,
		PLIThrottle:           sfu.DefaultPLIThrottleConfig,
		CongestionControl: CongestionControlConfig{
			Enabled:                   true,
			AllowPause:                false,
			StreamAllocator:           streamallocator.DefaultStreamAllocatorConfig,
			RemoteBWE:                 remotebwe.DefaultRemoteBWEConfig,
			UseSendSideBWEInterceptor: false,
			UseSendSideBWE:            false,
			SendSideBWE:               sendsidebwe.DefaultSendSideBWEConfig,
		},
	},
	Audio: sfu.DefaultAudioConfig,
	Video: VideoConfig{
		DynacastPauseDelay:   5 * time.Second,
		StreamTrackerManager: sfu.DefaultStreamTrackerManagerConfig,
	},
	Redis: redisLiveKit.RedisConfig{},
	Room: RoomConfig{
		AutoCreate: true,
		EnabledCodecs: []CodecSpec{
			{Mime: mime.MimeTypeOpus.String()},
			{Mime: mime.MimeTypeRED.String()},
			{Mime: mime.MimeTypeVP8.String()},
			{Mime: mime.MimeTypeH264.String()},
			{Mime: mime.MimeTypeVP9.String()},
			{Mime: mime.MimeTypeAV1.String()},
			{Mime: mime.MimeTypeRTX.String()},
		},
		EmptyTimeout:       5 * 60,
		DepartureTimeout:   20,
		CreateRoomEnabled:  true,
		CreateRoomTimeout:  10 * time.Second,
		CreateRoomAttempts: 3,
	},
	Limit: LimitConfig{
		MaxMetadataSize:              64000,
		MaxAttributesSize:            64000,
		MaxRoomNameLength:            256,
		MaxParticipantIdentityLength: 256,
		MaxParticipantNameLength:     256,
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
	SignalRelay: SignalRelayConfig{
		RetryTimeout:     7500 * time.Millisecond,
		MinRetryInterval: 500 * time.Millisecond,
		MaxRetryInterval: 4 * time.Second,
		StreamBufferSize: 1000,
		ConnectAttempts:  3,
	},
	PSRPC:  rpc.DefaultPSRPCConfig,
	Keys:   map[string]string{},
	Metric: metric.DefaultMetricConfig,
}

func NewConfig(confString string, strictMode bool, c *cli.Context, baseFlags []cli.Flag) (*Config, error) {
	// start with defaults
	marshalled, err := yaml.Marshal(&DefaultConfig)
	if err != nil {
		return nil, err
	}

	var conf Config
	err = yaml.Unmarshal(marshalled, &conf)
	if err != nil {
		return nil, err
	}

	if confString != "" {
		decoder := yaml.NewDecoder(strings.NewReader(confString))
		decoder.KnownFields(strictMode)
		if err := decoder.Decode(&conf); err != nil {
			return nil, fmt.Errorf("could not parse config: %v", err)
		}
	}

	if c != nil {
		if err := conf.updateFromCLI(c, baseFlags); err != nil {
			return nil, err
		}
	}

	if err := conf.RTC.Validate(conf.Development); err != nil {
		return nil, fmt.Errorf("could not validate RTC config: %v", err)
	}

	// expand env vars in filenames
	file, err := homedir.Expand(os.ExpandEnv(conf.KeyFile))
	if err != nil {
		return nil, err
	}
	conf.KeyFile = file

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

	if conf.LogLevel != "" {
		conf.Logging.Level = conf.LogLevel
	}
	if conf.Logging.Level == "" && conf.Development {
		conf.Logging.Level = "debug"
	}
	if conf.Logging.PionLevel != "" {
		if conf.Logging.ComponentLevels == nil {
			conf.Logging.ComponentLevels = map[string]string{}
		}
		conf.Logging.ComponentLevels["transport.pion"] = conf.Logging.PionLevel
		conf.Logging.ComponentLevels["pion"] = conf.Logging.PionLevel
	}

	// copy over legacy limits
	if conf.Room.MaxMetadataSize != 0 {
		conf.Limit.MaxMetadataSize = conf.Room.MaxMetadataSize
	}
	if conf.Room.MaxParticipantIdentityLength != 0 {
		conf.Limit.MaxParticipantIdentityLength = conf.Room.MaxParticipantIdentityLength
	}
	if conf.Room.MaxRoomNameLength != 0 {
		conf.Limit.MaxRoomNameLength = conf.Room.MaxRoomNameLength
	}

	return &conf, nil
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

type configNode struct {
	TypeNode  reflect.Value
	TagPrefix string
}

func (conf *Config) ToCLIFlagNames(existingFlags []cli.Flag) map[string]reflect.Value {
	existingFlagNames := map[string]bool{}
	for _, flag := range existingFlags {
		for _, flagName := range flag.Names() {
			existingFlagNames[flagName] = true
		}
	}

	flagNames := map[string]reflect.Value{}
	var currNode configNode
	nodes := []configNode{{reflect.ValueOf(conf).Elem(), ""}}
	for len(nodes) > 0 {
		currNode, nodes = nodes[0], nodes[1:]
		for i := 0; i < currNode.TypeNode.NumField(); i++ {
			// inspect yaml tag from struct field to get path
			field := currNode.TypeNode.Type().Field(i)
			yamlTagArray := strings.SplitN(field.Tag.Get("yaml"), ",", 2)
			yamlTag := yamlTagArray[0]
			isInline := false
			if len(yamlTagArray) > 1 && yamlTagArray[1] == "inline" {
				isInline = true
			}
			if (yamlTag == "" && (!isInline || currNode.TagPrefix == "")) || yamlTag == "-" {
				continue
			}
			yamlPath := yamlTag
			if currNode.TagPrefix != "" {
				if isInline {
					yamlPath = currNode.TagPrefix
				} else {
					yamlPath = fmt.Sprintf("%s.%s", currNode.TagPrefix, yamlTag)
				}
			}
			if existingFlagNames[yamlPath] {
				continue
			}

			// map flag name to value
			value := currNode.TypeNode.Field(i)
			if value.Kind() == reflect.Struct {
				nodes = append(nodes, configNode{value, yamlPath})
			} else {
				flagNames[yamlPath] = value
			}
		}
	}

	return flagNames
}

func (conf *Config) ValidateKeys() error {
	// prefer keyfile if set
	if conf.KeyFile != "" {
		var otherFilter os.FileMode = 0o007
		if st, err := os.Stat(conf.KeyFile); err != nil {
			return err
		} else if st.Mode().Perm()&otherFilter != 0o000 {
			return ErrKeyFileIncorrectPermission
		}
		f, err := os.Open(conf.KeyFile)
		if err != nil {
			return err
		}
		defer func() {
			_ = f.Close()
		}()
		decoder := yaml.NewDecoder(f)
		conf.Keys = map[string]string{}
		if err = decoder.Decode(conf.Keys); err != nil {
			return err
		}
	}

	if len(conf.Keys) == 0 {
		return ErrKeysNotSet
	}

	if !conf.Development {
		for key, secret := range conf.Keys {
			if len(secret) < 32 {
				logger.Errorw("secret is too short, should be at least 32 characters for security", nil, "apiKey", key)
			}
		}
	}
	return nil
}

func GenerateCLIFlags(existingFlags []cli.Flag, hidden bool) ([]cli.Flag, error) {
	blankConfig := &Config{}
	flags := make([]cli.Flag, 0)
	for name, value := range blankConfig.ToCLIFlagNames(existingFlags) {
		kind := value.Kind()
		if kind == reflect.Ptr {
			kind = value.Type().Elem().Kind()
		}

		var flag cli.Flag
		envVar := fmt.Sprintf("LIVEKIT_%s", strings.ToUpper(strings.Replace(name, ".", "_", -1)))

		switch kind {
		case reflect.Bool:
			flag = &cli.BoolFlag{
				Name:    name,
				EnvVars: []string{envVar},
				Usage:   generatedCLIFlagUsage,
				Hidden:  hidden,
			}
		case reflect.String:
			flag = &cli.StringFlag{
				Name:    name,
				EnvVars: []string{envVar},
				Usage:   generatedCLIFlagUsage,
				Hidden:  hidden,
			}
		case reflect.Int, reflect.Int32:
			flag = &cli.IntFlag{
				Name:    name,
				EnvVars: []string{envVar},
				Usage:   generatedCLIFlagUsage,
				Hidden:  hidden,
			}
		case reflect.Int64:
			flag = &cli.Int64Flag{
				Name:    name,
				EnvVars: []string{envVar},
				Usage:   generatedCLIFlagUsage,
				Hidden:  hidden,
			}
		case reflect.Uint8, reflect.Uint16, reflect.Uint32:
			flag = &cli.UintFlag{
				Name:    name,
				EnvVars: []string{envVar},
				Usage:   generatedCLIFlagUsage,
				Hidden:  hidden,
			}
		case reflect.Uint64:
			flag = &cli.Uint64Flag{
				Name:    name,
				EnvVars: []string{envVar},
				Usage:   generatedCLIFlagUsage,
				Hidden:  hidden,
			}
		case reflect.Float32:
			flag = &cli.Float64Flag{
				Name:    name,
				EnvVars: []string{envVar},
				Usage:   generatedCLIFlagUsage,
				Hidden:  hidden,
			}
		case reflect.Float64:
			flag = &cli.Float64Flag{
				Name:    name,
				EnvVars: []string{envVar},
				Usage:   generatedCLIFlagUsage,
				Hidden:  hidden,
			}
		case reflect.Slice:
			// TODO
			continue
		case reflect.Map:
			// TODO
			continue
		case reflect.Struct:
			// TODO
			continue
		default:
			return flags, fmt.Errorf("cli flag generation unsupported for config type: %s is a %s", name, kind.String())
		}

		flags = append(flags, flag)
	}

	return flags, nil
}

func (conf *Config) updateFromCLI(c *cli.Context, baseFlags []cli.Flag) error {
	generatedFlagNames := conf.ToCLIFlagNames(baseFlags)
	for _, flag := range c.App.Flags {
		flagName := flag.Names()[0]

		// the `c.App.Name != "test"` check is needed because `c.IsSet(...)` is always false in unit tests
		if !c.IsSet(flagName) && c.App.Name != "test" {
			continue
		}

		configValue, ok := generatedFlagNames[flagName]
		if !ok {
			continue
		}

		kind := configValue.Kind()
		if kind == reflect.Ptr {
			// instantiate value to be set
			configValue.Set(reflect.New(configValue.Type().Elem()))

			kind = configValue.Type().Elem().Kind()
			configValue = configValue.Elem()
		}

		switch kind {
		case reflect.Bool:
			configValue.SetBool(c.Bool(flagName))
		case reflect.String:
			configValue.SetString(c.String(flagName))
		case reflect.Int, reflect.Int32, reflect.Int64:
			configValue.SetInt(c.Int64(flagName))
		case reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			configValue.SetUint(c.Uint64(flagName))
		case reflect.Float32:
			configValue.SetFloat(c.Float64(flagName))
		case reflect.Float64:
			configValue.SetFloat(c.Float64(flagName))
		// case reflect.Slice:
		// 	// TODO
		// case reflect.Map:
		// 	// TODO
		default:
			return fmt.Errorf("unsupported generated cli flag type for config: %s is a %s", flagName, kind.String())
		}
	}

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
		conf.RTC.UDPPort.UnmarshalString(c.String("udp-port"))
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

// Note: only pass in logr.Logger with default depth
func SetLogger(l logger.Logger) {
	logger.SetLogger(l, "livekit")
}

func InitLoggerFromConfig(config *LoggingConfig) {
	logger.InitFromConfig(&config.Config, "livekit")
}
