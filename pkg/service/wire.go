//go:build wireinject
// +build wireinject

package service

import (
	"context"
	p2p_database "github.com/dTelecom/p2p-realtime-database"
	"github.com/google/wire"
	logging "github.com/ipfs/go-log/v2"
	"github.com/livekit/livekit-server/pkg/clientconfiguration"
	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/telemetry"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/egress"
	"github.com/livekit/protocol/livekit"
	redisLiveKit "github.com/livekit/protocol/redis"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/protocol/webhook"
	"github.com/livekit/psrpc"
	"github.com/pion/turn/v2"
	"github.com/pkg/errors"
	"github.com/redis/go-redis/v9"
)

func InitializeServer(conf *config.Config, currentNode routing.LocalNode) (*LivekitServer, error) {
	wire.Build(
		getNodeID,
		createRedisClient,
		getDatabaseConfiguration,
		createStore,
		wire.Bind(new(ServiceStore), new(ObjectStore)),
		createKeyProvider,
		createKeyPublicKeyProvider,
		createWebhookNotifier,
		createClientConfiguration,
		routing.CreateRouter,
		getRoomConf,
		config.DefaultAPIConfig,
		createMainDatabaseP2P,
		createParticipantCounter,
		//wire.Bind(new(MainP2PDatabase), new(*p2p_database.DB)),
		wire.Bind(new(routing.MessageRouter), new(routing.Router)),
		wire.Bind(new(livekit.RoomService), new(*RoomService)),
		telemetry.NewAnalyticsService,
		telemetry.NewTelemetryService,
		getMessageBus,
		NewIOInfoService,
		getEgressClient,
		egress.NewRedisRPCClient,
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

func createParticipantCounter(mainDatabase *p2p_database.DB) *ParticipantCounter {
	return NewParticipantCounter(mainDatabase)
}

func getDatabaseConfiguration(conf *config.Config) p2p_database.Config {
	return p2p_database.Config{
		PeerListenPort:          conf.Ethereum.P2pNodePort,
		EthereumNetworkHost:     conf.Ethereum.NetworkHost,
		EthereumNetworkKey:      conf.Ethereum.NetworkKey,
		EthereumContractAddress: conf.Ethereum.ContractAddress,
		WalletPrivateKey:        conf.Ethereum.WalletPrivateKey,
		DatabaseName:            conf.Ethereum.P2pMainDatabaseName,
	}
}

func createMainDatabaseP2P(conf p2p_database.Config) (*p2p_database.DB, error) {
	db, err := p2p_database.Connect(context.Background(), conf, logging.Logger("db"))
	if err != nil {
		return nil, errors.Wrap(err, "create main p2p db")
	}
	return db, nil
}

func getNodeID(currentNode routing.LocalNode) livekit.NodeID {
	return livekit.NodeID(currentNode.Id)
}

func createKeyProvider(conf *config.Config) (auth.KeyProvider, error) {
	return createKeyPublicKeyProvider(conf)
}

func createKeyPublicKeyProvider(conf *config.Config) (auth.KeyProviderPublicKey, error) {
	contract, err := p2p_database.NewEthSmartContract(p2p_database.Config{
		EthereumNetworkHost:     conf.Ethereum.NetworkHost,
		EthereumNetworkKey:      conf.Ethereum.NetworkKey,
		EthereumContractAddress: conf.Ethereum.ContractAddress,
	}, nil)

	if err != nil {
		return nil, errors.Wrap(err, "try create contract")
	}

	return auth.NewEthKeyProvider(*contract, conf.Ethereum.WalletAddress, conf.Ethereum.WalletPrivateKey), nil
}

func createWebhookNotifier(conf *config.Config, provider auth.KeyProvider) (webhook.Notifier, error) {
	wc := conf.WebHook
	if len(wc.URLs) == 0 {
		return nil, nil
	}
	secret := provider.GetSecret(wc.APIKey)
	if secret == "" {
		return nil, ErrWebHookMissingAPIKey
	}

	return webhook.NewNotifier(wc.APIKey, secret, wc.URLs), nil
}

func createRedisClient(conf *config.Config) (redis.UniversalClient, error) {
	if !conf.Redis.IsConfigured() {
		return nil, nil
	}
	return redisLiveKit.GetRedisClient(&conf.Redis)
}

func createStore(p2pDbConfig p2p_database.Config, nodeID livekit.NodeID, participantCounter *ParticipantCounter) ObjectStore {
	return NewLocalStore(nodeID, p2pDbConfig, participantCounter)
}

func getMessageBus(rc redis.UniversalClient) psrpc.MessageBus {
	if rc == nil {
		return psrpc.NewLocalMessageBus()
	}
	return psrpc.NewRedisMessageBus(rc)
}

func getEgressClient(conf *config.Config, nodeID livekit.NodeID, bus psrpc.MessageBus) (rpc.EgressClient, error) {
	if conf.Egress.UsePsRPC {
		return rpc.NewEgressClient(nodeID, bus)
	}

	return nil, nil
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
