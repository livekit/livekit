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

//go:build wireinject
// +build wireinject

package service

import (
	"fmt"
	"os"

	"github.com/google/wire"
	"github.com/pion/turn/v4"
	"github.com/pkg/errors"
	"github.com/redis/go-redis/v9"
	"gopkg.in/yaml.v3"

	"github.com/livekit/livekit-server/pkg/agent"
	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/telemetry"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	redisLiveKit "github.com/livekit/protocol/redis"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/protocol/webhook"
	"github.com/livekit/psrpc"
)

func InitializeServer(conf *config.Config, currentNode routing.LocalNode) (*LivekitServer, error) {
	wire.Build(
		getNodeID,
		createRedisClient,
		createStore,
		wire.Bind(new(ServiceStore), new(ObjectStore)),
		createKeyProvider,
		createWebhookNotifier,
		createForwardStats,
		getNodeStatsConfig,
		routing.CreateRouter,
		getLimitConf,
		config.DefaultAPIConfig,
		wire.Bind(new(routing.MessageRouter), new(routing.Router)),
		wire.Bind(new(livekit.RoomService), new(*RoomService)),
		telemetry.NewAnalyticsService,
		telemetry.NewTelemetryService,
		getMessageBus,
		NewIOInfoService,
		wire.Bind(new(IOClient), new(*IOInfoService)),
		rpc.NewEgressClient,
		rpc.NewIngressClient,
		getEgressStore,
		NewEgressLauncher,
		NewEgressService,
		getIngressStore,
		getIngressConfig,
		NewIngressService,
		rpc.NewSIPClient,
		getSIPStore,
		getSIPConfig,
		NewSIPService,
		NewRoomAllocator,
		NewRoomService,
		NewRTCService,
		NewWHIPService,
		NewAgentService,
		NewAgentDispatchService,
		getAgentConfig,
		agent.NewAgentClient,
		getAgentStore,
		getSignalRelayConfig,
		NewDefaultSignalServer,
		routing.NewSignalClient,
		getRoomConfig,
		routing.NewRoomManagerClient,
		rpc.NewKeepalivePubSub,
		getPSRPCConfig,
		getPSRPCClientParams,
		rpc.NewTopicFormatter,
		rpc.NewTypedRoomClient,
		rpc.NewTypedParticipantClient,
		rpc.NewTypedWHIPParticipantClient,
		rpc.NewTypedAgentDispatchInternalClient,
		NewLocalRoomManager,
		NewTURNAuthHandler,
		getTURNAuthHandlerFunc,
		newInProcessTurnServer,
		utils.NewDefaultTimedVersionGenerator,
		NewLivekitServer,
	)
	return &LivekitServer{}, nil
}

func InitializeRouter(conf *config.Config, currentNode routing.LocalNode) (routing.Router, error) {
	wire.Build(
		createRedisClient,
		getNodeID,
		getMessageBus,
		getSignalRelayConfig,
		getPSRPCConfig,
		getPSRPCClientParams,
		routing.NewSignalClient,
		getRoomConfig,
		routing.NewRoomManagerClient,
		rpc.NewKeepalivePubSub,
		getNodeStatsConfig,
		routing.CreateRouter,
	)

	return nil, nil
}

func getNodeID(currentNode routing.LocalNode) livekit.NodeID {
	return currentNode.NodeID()
}

func createKeyProvider(conf *config.Config) (auth.KeyProvider, error) {
	// prefer keyfile if set
	if conf.KeyFile != "" {
		var otherFilter os.FileMode = 0007
		if st, err := os.Stat(conf.KeyFile); err != nil {
			return nil, err
		} else if st.Mode().Perm()&otherFilter != 0000 {
			return nil, fmt.Errorf("key file others permissions must be set to 0")
		}
		f, err := os.Open(conf.KeyFile)
		if err != nil {
			return nil, err
		}
		defer func() {
			_ = f.Close()
		}()
		decoder := yaml.NewDecoder(f)
		if err = decoder.Decode(conf.Keys); err != nil {
			return nil, err
		}
	}

	if len(conf.Keys) == 0 {
		return nil, errors.New("one of key-file or keys must be provided in order to support a secure installation")
	}

	return auth.NewFileBasedKeyProviderFromMap(conf.Keys), nil
}

func createWebhookNotifier(conf *config.Config, provider auth.KeyProvider) (webhook.QueuedNotifier, error) {
	wc := conf.WebHook

	secret := provider.GetSecret(wc.APIKey)
	if secret == "" && len(wc.URLs) > 0 {
		return nil, ErrWebHookMissingAPIKey
	}

	return webhook.NewDefaultNotifier(wc, provider)
}

func createRedisClient(conf *config.Config) (redis.UniversalClient, error) {
	if !conf.Redis.IsConfigured() {
		return nil, nil
	}
	return redisLiveKit.GetRedisClient(&conf.Redis)
}

func createStore(rc redis.UniversalClient) ObjectStore {
	if rc != nil {
		return NewRedisStore(rc)
	}
	return NewLocalStore()
}

func getMessageBus(rc redis.UniversalClient) psrpc.MessageBus {
	if rc == nil {
		return psrpc.NewLocalMessageBus()
	}
	return psrpc.NewRedisMessageBus(rc)
}

func getEgressStore(s ObjectStore) EgressStore {
	switch store := s.(type) {
	case *RedisStore:
		return store
	default:
		return nil
	}
}

func getIngressStore(s ObjectStore) IngressStore {
	switch store := s.(type) {
	case *RedisStore:
		return store
	default:
		return nil
	}
}

func getAgentStore(s ObjectStore) AgentStore {
	switch store := s.(type) {
	case *RedisStore:
		return store
	case *LocalStore:
		return store
	default:
		return nil
	}
}

func getIngressConfig(conf *config.Config) *config.IngressConfig {
	return &conf.Ingress
}

func getSIPStore(s ObjectStore) SIPStore {
	switch store := s.(type) {
	case *RedisStore:
		return store
	default:
		return nil
	}
}

func getSIPConfig(conf *config.Config) *config.SIPConfig {
	return &conf.SIP
}

func getLimitConf(config *config.Config) config.LimitConfig {
	return config.Limit
}

func getRoomConfig(config *config.Config) config.RoomConfig {
	return config.Room
}

func getSignalRelayConfig(config *config.Config) config.SignalRelayConfig {
	return config.SignalRelay
}

func getPSRPCConfig(config *config.Config) rpc.PSRPCConfig {
	return config.PSRPC
}

func getPSRPCClientParams(config rpc.PSRPCConfig, bus psrpc.MessageBus) rpc.ClientParams {
	return rpc.NewClientParams(config, bus, logger.GetLogger(), rpc.PSRPCMetricsObserver{})
}

func createForwardStats(conf *config.Config) *sfu.ForwardStats {
	if conf.RTC.ForwardStats.SummaryInterval == 0 || conf.RTC.ForwardStats.ReportInterval == 0 || conf.RTC.ForwardStats.ReportWindow == 0 {
		return nil
	}
	return sfu.NewForwardStats(conf.RTC.ForwardStats.SummaryInterval, conf.RTC.ForwardStats.ReportInterval, conf.RTC.ForwardStats.ReportWindow)
}

func newInProcessTurnServer(conf *config.Config, authHandler turn.AuthHandler) (*turn.Server, error) {
	return NewTurnServer(conf, authHandler, false)
}

func getNodeStatsConfig(config *config.Config) config.NodeStatsConfig {
	return config.NodeStats
}

func getAgentConfig(config *config.Config) agent.Config {
	return config.Agents
}
