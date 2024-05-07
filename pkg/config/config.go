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
	"github.com/pion/webrtc/v3"
	"github.com/pkg/errors"
	"github.com/urfave/cli/v2"
	"gopkg.in/yaml.v3"

	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
	"github.com/livekit/protocol/logger"
	redisLiveKit "github.com/livekit/protocol/redis"
	"github.com/livekit/protocol/rpc"
)

type (
	CongestionControlProbeMode string
	StreamTrackerType          string
)

const (
	generatedCLIFlagUsage = "generated"

	CongestionControlProbeModePadding CongestionControlProbeMode = "padding"
	CongestionControlProbeModeMedia   CongestionControlProbeMode = "media"

	StreamTrackerTypePacket StreamTrackerType = "packet"
	StreamTrackerTypeFrame  StreamTrackerType = "frame"

	StatsUpdateInterval                  = time.Second * 10
	TelemetryStatsUpdateInterval         = time.Second * 30
	TelemetryNonMediaStatsUpdateInterval = time.Minute * 5
)

var (
	ErrKeyFileIncorrectPermission = errors.New("key file others permissions must be set to 0")
	ErrKeysNotSet                 = errors.New("one of key-file or keys must be provided")
)

type Config struct {
	Port           uint32                   `yaml:"port,omitempty"`
	BindAddresses  []string                 `yaml:"bind_addresses,omitempty"`
	PrometheusPort uint32                   `yaml:"prometheus_port,omitempty"`
	RTC            RTCConfig                `yaml:"rtc,omitempty"`
	Redis          redisLiveKit.RedisConfig `yaml:"redis,omitempty"`
	Audio          AudioConfig              `yaml:"audio,omitempty"`
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
}

type RTCConfig struct {
	rtcconfig.RTCConfig `yaml:",inline"`

	TURNServers []TURNServer `yaml:"turn_servers,omitempty"`

	StrictACKs bool `yaml:"strict_acks,omitempty"`

	// Deprecated: use PacketBufferSizeVideo and PacketBufferSizeAudio
	PacketBufferSize int `yaml:"packet_buffer_size,omitempty"`
	// Number of packets to buffer for NACK - video
	PacketBufferSizeVideo int `yaml:"packet_buffer_size_video,omitempty"`
	// Number of packets to buffer for NACK - audio
	PacketBufferSizeAudio int `yaml:"packet_buffer_size_audio,omitempty"`

	// Throttle periods for pli/fir rtcp packets
	PLIThrottle PLIThrottleConfig `yaml:"pli_throttle,omitempty"`

	CongestionControl CongestionControlConfig `yaml:"congestion_control,omitempty"`

	// allow TCP and TURN/TLS fallback
	AllowTCPFallback *bool `yaml:"allow_tcp_fallback,omitempty"`

	// force a reconnect on a publication error
	ReconnectOnPublicationError *bool `yaml:"reconnect_on_publication_error,omitempty"`

	// force a reconnect on a subscription error
	ReconnectOnSubscriptionError *bool `yaml:"reconnect_on_subscription_error,omitempty"`

	// force a reconnect on a data channel error
	ReconnectOnDataChannelError *bool `yaml:"reconnect_on_data_channel_error,omitempty"`

	// max number of bytes to buffer for data channel. 0 means unlimited
	DataChannelMaxBufferedAmount uint64 `yaml:"data_channel_max_buffered_amount,omitempty"`
}

type TURNServer struct {
	Host       string `yaml:"host,omitempty"`
	Port       int    `yaml:"port,omitempty"`
	Protocol   string `yaml:"protocol,omitempty"`
	Username   string `yaml:"username,omitempty"`
	Credential string `yaml:"credential,omitempty"`
}

type PLIThrottleConfig struct {
	LowQuality  time.Duration `yaml:"low_quality,omitempty"`
	MidQuality  time.Duration `yaml:"mid_quality,omitempty"`
	HighQuality time.Duration `yaml:"high_quality,omitempty"`
}

type CongestionControlProbeConfig struct {
	BaseInterval  time.Duration `yaml:"base_interval,omitempty"`
	BackoffFactor float64       `yaml:"backoff_factor,omitempty"`
	MaxInterval   time.Duration `yaml:"max_interval,omitempty"`

	SettleWait    time.Duration `yaml:"settle_wait,omitempty"`
	SettleWaitMax time.Duration `yaml:"settle_wait_max,omitempty"`

	TrendWait time.Duration `yaml:"trend_wait,omitempty"`

	OveragePct             int64         `yaml:"overage_pct,omitempty"`
	MinBps                 int64         `yaml:"min_bps,omitempty"`
	MinDuration            time.Duration `yaml:"min_duration,omitempty"`
	MaxDuration            time.Duration `yaml:"max_duration,omitempty"`
	DurationOverflowFactor float64       `yaml:"duration_overflow_factor,omitempty"`
	DurationIncreaseFactor float64       `yaml:"duration_increase_factor,omitempty"`
}

type CongestionControlChannelObserverConfig struct {
	EstimateRequiredSamples        int           `yaml:"estimate_required_samples,omitempty"`
	EstimateRequiredSamplesMin     int           `yaml:"estimate_required_samples_min,omitempty"`
	EstimateDownwardTrendThreshold float64       `yaml:"estimate_downward_trend_threshold,omitempty"`
	EstimateDownwardTrendMaxWait   time.Duration `yaml:"estimate_downward_trend_max_wait,omitempty"`
	EstimateCollapseThreshold      time.Duration `yaml:"estimate_collapse_threshold,omitempty"`
	EstimateValidityWindow         time.Duration `yaml:"estimate_validity_window,omitempty"`
	NackWindowMinDuration          time.Duration `yaml:"nack_window_min_duration,omitempty"`
	NackWindowMaxDuration          time.Duration `yaml:"nack_window_max_duration,omitempty"`
	NackRatioThreshold             float64       `yaml:"nack_ratio_threshold,omitempty"`
}

type CongestionControlConfig struct {
	Enabled                          bool                                   `yaml:"enabled,omitempty"`
	AllowPause                       bool                                   `yaml:"allow_pause,omitempty"`
	NackRatioAttenuator              float64                                `yaml:"nack_ratio_attenuator,omitempty"`
	ExpectedUsageThreshold           float64                                `yaml:"expected_usage_threshold,omitempty"`
	UseSendSideBWE                   bool                                   `yaml:"send_side_bandwidth_estimation,omitempty"`
	ProbeMode                        CongestionControlProbeMode             `yaml:"probe_mode,omitempty"`
	MinChannelCapacity               int64                                  `yaml:"min_channel_capacity,omitempty"`
	ProbeConfig                      CongestionControlProbeConfig           `yaml:"probe_config,omitempty"`
	ChannelObserverProbeConfig       CongestionControlChannelObserverConfig `yaml:"channel_observer_probe_config,omitempty"`
	ChannelObserverNonProbeConfig    CongestionControlChannelObserverConfig `yaml:"channel_observer_non_probe_config,omitempty"`
	DisableEstimationUnmanagedTracks bool                                   `yaml:"disable_etimation_unmanaged_tracks,omitempty"`
}

type AudioConfig struct {
	// minimum level to be considered active, 0-127, where 0 is loudest
	ActiveLevel uint8 `yaml:"active_level,omitempty"`
	// percentile to measure, a participant is considered active if it has exceeded the ActiveLevel more than
	// MinPercentile% of the time
	MinPercentile uint8 `yaml:"min_percentile,omitempty"`
	// interval to update clients, in ms
	UpdateInterval uint32 `yaml:"update_interval,omitempty"`
	// smoothing for audioLevel values sent to the client.
	// audioLevel will be an average of `smooth_intervals`, 0 to disable
	SmoothIntervals uint32 `yaml:"smooth_intervals,omitempty"`
	// enable red encoding downtrack for opus only audio up track
	ActiveREDEncoding bool `yaml:"active_red_encoding,omitempty"`
	// enable proxying weakest subscriber loss to publisher in RTCP Receiver Report
	EnableLossProxying bool `yaml:"enable_loss_proxying,omitempty"`
}

type StreamTrackerPacketConfig struct {
	SamplesRequired uint32        `yaml:"samples_required,omitempty"` // number of samples needed per cycle
	CyclesRequired  uint32        `yaml:"cycles_required,omitempty"`  // number of cycles needed to be active
	CycleDuration   time.Duration `yaml:"cycle_duration,omitempty"`
}

type StreamTrackerFrameConfig struct {
	MinFPS float64 `yaml:"min_fps,omitempty"`
}

type StreamTrackerConfig struct {
	StreamTrackerType     StreamTrackerType                   `yaml:"stream_tracker_type,omitempty"`
	BitrateReportInterval map[int32]time.Duration             `yaml:"bitrate_report_interval,omitempty"`
	PacketTracker         map[int32]StreamTrackerPacketConfig `yaml:"packet_tracker,omitempty"`
	FrameTracker          map[int32]StreamTrackerFrameConfig  `yaml:"frame_tracker,omitempty"`
}

type StreamTrackersConfig struct {
	Video       StreamTrackerConfig `yaml:"video,omitempty"`
	Screenshare StreamTrackerConfig `yaml:"screenshare,omitempty"`
}

type PlayoutDelayConfig struct {
	Enabled bool `yaml:"enabled,omitempty"`
	Min     int  `yaml:"min,omitempty"`
	Max     int  `yaml:"max,omitempty"`
}

type VideoConfig struct {
	DynacastPauseDelay time.Duration        `yaml:"dynacast_pause_delay,omitempty"`
	StreamTracker      StreamTrackersConfig `yaml:"stream_tracker,omitempty"`
}

type RoomConfig struct {
	// enable rooms to be automatically created
	AutoCreate                   bool               `yaml:"auto_create,omitempty"`
	EnabledCodecs                []CodecSpec        `yaml:"enabled_codecs,omitempty"`
	MaxParticipants              uint32             `yaml:"max_participants,omitempty"`
	EmptyTimeout                 uint32             `yaml:"empty_timeout,omitempty"`
	DepartureTimeout             uint32             `yaml:"departure_timeout,omitempty"`
	EnableRemoteUnmute           bool               `yaml:"enable_remote_unmute,omitempty"`
	MaxMetadataSize              uint32             `yaml:"max_metadata_size,omitempty"`
	PlayoutDelay                 PlayoutDelayConfig `yaml:"playout_delay,omitempty"`
	SyncStreams                  bool               `yaml:"sync_streams,omitempty"`
	MaxRoomNameLength            int                `yaml:"max_room_name_length,omitempty"`
	MaxParticipantIdentityLength int                `yaml:"max_participant_identity_length,omitempty"`
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
		StrictACKs:            true,
		PLIThrottle: PLIThrottleConfig{
			LowQuality:  500 * time.Millisecond,
			MidQuality:  time.Second,
			HighQuality: time.Second,
		},
		CongestionControl: CongestionControlConfig{
			Enabled:                true,
			AllowPause:             false,
			NackRatioAttenuator:    0.4,
			ExpectedUsageThreshold: 0.95,
			ProbeMode:              CongestionControlProbeModePadding,
			ProbeConfig: CongestionControlProbeConfig{
				BaseInterval:  3 * time.Second,
				BackoffFactor: 1.5,
				MaxInterval:   2 * time.Minute,

				SettleWait:    250 * time.Millisecond,
				SettleWaitMax: 10 * time.Second,

				TrendWait: 2 * time.Second,

				OveragePct:             120,
				MinBps:                 200_000,
				MinDuration:            200 * time.Millisecond,
				MaxDuration:            20 * time.Second,
				DurationOverflowFactor: 1.25,
				DurationIncreaseFactor: 1.5,
			},
			ChannelObserverProbeConfig: CongestionControlChannelObserverConfig{
				EstimateRequiredSamples:        3,
				EstimateRequiredSamplesMin:     3,
				EstimateDownwardTrendThreshold: 0.0,
				EstimateDownwardTrendMaxWait:   5 * time.Second,
				EstimateCollapseThreshold:      0,
				EstimateValidityWindow:         10 * time.Second,
				NackWindowMinDuration:          500 * time.Millisecond,
				NackWindowMaxDuration:          1 * time.Second,
				NackRatioThreshold:             0.04,
			},
			ChannelObserverNonProbeConfig: CongestionControlChannelObserverConfig{
				EstimateRequiredSamples:        12,
				EstimateRequiredSamplesMin:     8,
				EstimateDownwardTrendThreshold: -0.6,
				EstimateDownwardTrendMaxWait:   5 * time.Second,
				EstimateCollapseThreshold:      500 * time.Millisecond,
				EstimateValidityWindow:         10 * time.Second,
				NackWindowMinDuration:          2 * time.Second,
				NackWindowMaxDuration:          3 * time.Second,
				NackRatioThreshold:             0.08,
			},
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
		StreamTracker: StreamTrackersConfig{
			Video: StreamTrackerConfig{
				StreamTrackerType: StreamTrackerTypePacket,
				BitrateReportInterval: map[int32]time.Duration{
					0: 1 * time.Second,
					1: 1 * time.Second,
					2: 1 * time.Second,
				},
				PacketTracker: map[int32]StreamTrackerPacketConfig{
					0: {
						SamplesRequired: 1,
						CyclesRequired:  4,
						CycleDuration:   500 * time.Millisecond,
					},
					1: {
						SamplesRequired: 5,
						CyclesRequired:  20,
						CycleDuration:   500 * time.Millisecond,
					},
					2: {
						SamplesRequired: 5,
						CyclesRequired:  20,
						CycleDuration:   500 * time.Millisecond,
					},
				},
				FrameTracker: map[int32]StreamTrackerFrameConfig{
					0: {
						MinFPS: 5.0,
					},
					1: {
						MinFPS: 5.0,
					},
					2: {
						MinFPS: 5.0,
					},
				},
			},
			Screenshare: StreamTrackerConfig{
				StreamTrackerType: StreamTrackerTypePacket,
				BitrateReportInterval: map[int32]time.Duration{
					0: 4 * time.Second,
					1: 4 * time.Second,
					2: 4 * time.Second,
				},
				PacketTracker: map[int32]StreamTrackerPacketConfig{
					0: {
						SamplesRequired: 1,
						CyclesRequired:  1,
						CycleDuration:   2 * time.Second,
					},
					1: {
						SamplesRequired: 1,
						CyclesRequired:  1,
						CycleDuration:   2 * time.Second,
					},
					2: {
						SamplesRequired: 1,
						CyclesRequired:  1,
						CycleDuration:   2 * time.Second,
					},
				},
				FrameTracker: map[int32]StreamTrackerFrameConfig{
					0: {
						MinFPS: 0.5,
					},
					1: {
						MinFPS: 0.5,
					},
					2: {
						MinFPS: 0.5,
					},
				},
			},
		},
	},
	Redis: redisLiveKit.RedisConfig{},
	Room: RoomConfig{
		AutoCreate: true,
		EnabledCodecs: []CodecSpec{
			{Mime: webrtc.MimeTypeOpus},
			{Mime: "audio/red"},
			{Mime: webrtc.MimeTypeVP8},
			{Mime: webrtc.MimeTypeH264},
			{Mime: webrtc.MimeTypeVP9},
			{Mime: webrtc.MimeTypeAV1},
		},
		EmptyTimeout:                 5 * 60,
		DepartureTimeout:             20,
		MaxRoomNameLength:            256,
		MaxParticipantIdentityLength: 256,
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
	},
	PSRPC: rpc.DefaultPSRPCConfig,
	Keys:  map[string]string{},
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
				Name:   name,
				Usage:  generatedCLIFlagUsage,
				Hidden: hidden,
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
