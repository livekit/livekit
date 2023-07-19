//go:build wireinject
// +build wireinject

package service

import (
	"fmt"
	"os"

	"github.com/google/wire"
	"github.com/pion/turn/v2"
	"github.com/pkg/errors"
	"github.com/redis/go-redis/v9"
	"gopkg.in/yaml.v3"

	"github.com/livekit/livekit-server/pkg/clientconfiguration"
	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/telemetry"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
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
		createClientConfiguration,
		routing.CreateRouter,
		getRoomConf,
		config.DefaultAPIConfig,
		wire.Bind(new(routing.MessageRouter), new(routing.Router)),
		wire.Bind(new(livekit.RoomService), new(*RoomService)),
		telemetry.NewAnalyticsService,
		telemetry.NewTelemetryService,
		getMessageBus,
		NewIOInfoService,
		rpc.NewEgressClient,
		getEgressStore,
		NewEgressLauncher,
		NewEgressService,
		rpc.NewIngressClient,
		getIngressStore,
		getIngressConfig,
		NewIngressService,
		NewRoomAllocator,
		NewRoomService,
		NewRTCService,
		getSignalRelayConfig,
		NewDefaultSignalServer,
		routing.NewSignalClient,
		NewLocalRoomManager,
		newTurnAuthHandler,
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
		routing.NewSignalClient,
		routing.CreateRouter,
	)

	return nil, nil
}

func getNodeID(currentNode routing.LocalNode) livekit.NodeID {
	return livekit.NodeID(currentNode.Id)
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
	if len(wc.URLs) == 0 {
		return nil, nil
	}
	secret := provider.GetSecret(wc.APIKey)
	if secret == "" {
		return nil, ErrWebHookMissingAPIKey
	}

	return webhook.NewDefaultNotifier(wc.APIKey, secret, wc.URLs), nil
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

func getIngressConfig(conf *config.Config) *config.IngressConfig {
	return &conf.Ingress
}

func createClientConfiguration() clientconfiguration.ClientConfigurationManager {
	return clientconfiguration.NewStaticClientConfigurationManager(clientconfiguration.StaticConfigurations)
}

func getRoomConf(config *config.Config) config.RoomConfig {
	return config.Room
}

func getSignalRelayConfig(config *config.Config) config.SignalRelayConfig {
	return config.SignalRelay
}

func newInProcessTurnServer(conf *config.Config, authHandler turn.AuthHandler) (*turn.Server, error) {
	return NewTurnServer(conf, authHandler, false)
}
