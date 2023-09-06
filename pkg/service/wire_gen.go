// Code generated by Wire. DO NOT EDIT.

//go:generate go run github.com/google/wire/cmd/wire
//go:build !wireinject
// +build !wireinject

package service

import (
	"context"
	"github.com/dTelecom/p2p-realtime-database"
	"github.com/livekit/livekit-server"
	"github.com/livekit/livekit-server/pkg/clientconfiguration"
	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/telemetry"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/egress"
	livekit2 "github.com/livekit/protocol/livekit"
	redis2 "github.com/livekit/protocol/redis"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/protocol/webhook"
	"github.com/livekit/psrpc"
	"github.com/oschwald/geoip2-golang"
	"github.com/pion/turn/v2"
	"github.com/pkg/errors"
	"github.com/redis/go-redis/v9"
)

import (
	_ "net/http/pprof"
)

// Injectors from wire.go:

func InitializeServer(conf *config.Config, currentNode routing.LocalNode) (*LivekitServer, error) {
	roomConfig := getRoomConf(conf)
	apiConfig := config.DefaultAPIConfig()
	universalClient, err := createRedisClient(conf)
	if err != nil {
		return nil, err
	}
	nodeID := getNodeID(currentNode)
	messageBus := getMessageBus(universalClient)
	signalRelayConfig := getSignalRelayConfig(conf)
	signalClient, err := routing.NewSignalClient(nodeID, messageBus, signalRelayConfig)
	if err != nil {
		return nil, err
	}
	p2p_databaseConfig := GetDatabaseConfiguration(conf)
	db, err := CreateMainDatabaseP2P(p2p_databaseConfig, conf)
	if err != nil {
		return nil, err
	}
	router := routing.CreateRouter(conf, universalClient, currentNode, signalClient, db)
	participantCounter, err := createParticipantCounter(db, conf)
	if err != nil {
		return nil, err
	}
	reader, err := createGeoIP()
	if err != nil {
		return nil, err
	}
	nodeProvider := CreateNodeProvider(reader, conf, db)
	objectStore := createStore(db, p2p_databaseConfig, nodeID, participantCounter, conf, nodeProvider)
	roomAllocator, err := NewRoomAllocator(conf, router, objectStore)
	if err != nil {
		return nil, err
	}
	egressClient, err := getEgressClient(conf, nodeID, messageBus)
	if err != nil {
		return nil, err
	}
	rpcClient := egress.NewRedisRPCClient(nodeID, universalClient)
	egressStore := getEgressStore(objectStore)
	ethSmartContract, err := createSmartContractClient(conf)
	if err != nil {
		return nil, err
	}
	keyProvider, err := createKeyProvider(conf, ethSmartContract)
	if err != nil {
		return nil, err
	}
	notifier, err := createWebhookNotifier(conf, keyProvider)
	if err != nil {
		return nil, err
	}
	analyticsService := telemetry.NewAnalyticsService(conf, currentNode)
	telemetryService := telemetry.NewTelemetryService(notifier, analyticsService)
	rtcEgressLauncher := NewEgressLauncher(egressClient, rpcClient, egressStore, telemetryService)
	roomService, err := NewRoomService(roomConfig, apiConfig, router, roomAllocator, objectStore, rtcEgressLauncher)
	if err != nil {
		return nil, err
	}
	egressService := NewEgressService(egressClient, rpcClient, objectStore, egressStore, roomService, telemetryService, rtcEgressLauncher)
	ingressConfig := getIngressConfig(conf)
	ingressClient, err := rpc.NewIngressClient(nodeID, messageBus)
	if err != nil {
		return nil, err
	}
	ingressStore := getIngressStore(objectStore)
	ingressService := NewIngressService(ingressConfig, nodeID, messageBus, ingressClient, ingressStore, roomService, telemetryService)
	rtcService := NewRTCService(conf, roomAllocator, objectStore, router, currentNode, telemetryService)
	keyProviderPublicKey, err := createKeyPublicKeyProvider(conf, ethSmartContract)
	if err != nil {
		return nil, err
	}
	clientConfigurationManager := createClientConfiguration()
	timedVersionGenerator := utils.NewDefaultTimedVersionGenerator()
	clientProvider := createClientProvider(ethSmartContract, db)
	trafficManager := createTrafficManager(db, conf)
	roomManager, err := NewLocalRoomManager(conf, objectStore, currentNode, router, telemetryService, clientConfigurationManager, rtcEgressLauncher, timedVersionGenerator, trafficManager)
	if err != nil {
		return nil, err
	}
	signalServer, err := NewDefaultSignalServer(currentNode, messageBus, signalRelayConfig, router, roomManager)
	if err != nil {
		return nil, err
	}
	authHandler := newTurnAuthHandler(objectStore)
	server, err := newInProcessTurnServer(conf, authHandler)
	if err != nil {
		return nil, err
	}
	relevantNodesHandler := createRelevantNodesHandler(conf, nodeProvider)
	livekitServer, err := NewLivekitServer(conf, roomService, egressService, ingressService, rtcService, keyProviderPublicKey, router, roomManager, signalServer, server, currentNode, clientProvider, participantCounter, nodeProvider, db, relevantNodesHandler)
	if err != nil {
		return nil, err
	}
	return livekitServer, nil
}

// wire.go:

func createRelevantNodesHandler(conf *config.Config, nodeProvider *NodeProvider) *RelevantNodesHandler {
	return NewRelevantNodesHandler(nodeProvider, conf.LoggingP2P)
}

func createGeoIP() (*geoip2.Reader, error) {
	return geoip2.FromBytes(livekit.MixmindDatabase)
}

func CreateNodeProvider(geo *geoip2.Reader, config2 *config.Config, db *p2p_database.DB) *NodeProvider {
	return NewNodeProvider(db, geo, config2.LoggingP2P)
}

func createClientProvider(contract *p2p_database.EthSmartContract, db *p2p_database.DB) *ClientProvider {
	return NewClientProvider(db, contract)
}

func createSmartContractClient(conf *config.Config) (*p2p_database.EthSmartContract, error) {
	contract, err := p2p_database.NewEthSmartContract(p2p_database.Config{
		EthereumNetworkHost:     conf.Ethereum.NetworkHost,
		EthereumNetworkKey:      conf.Ethereum.NetworkKey,
		EthereumContractAddress: conf.Ethereum.ContractAddress,
	}, conf.LoggingP2P)

	if err != nil {
		return nil, errors.Wrap(err, "try create contract")
	}

	return contract, nil
}

func createParticipantCounter(mainDatabase *p2p_database.DB, conf *config.Config) (*ParticipantCounter, error) {
	return NewParticipantCounter(mainDatabase, conf.LoggingP2P)
}

func GetDatabaseConfiguration(conf *config.Config) p2p_database.Config {
	return p2p_database.Config{
		PeerListenPort:          conf.Ethereum.P2pNodePort,
		EthereumNetworkHost:     conf.Ethereum.NetworkHost,
		EthereumNetworkKey:      conf.Ethereum.NetworkKey,
		EthereumContractAddress: conf.Ethereum.ContractAddress,
		WalletPrivateKey:        conf.Ethereum.WalletPrivateKey,
		DatabaseName:            conf.Ethereum.P2pMainDatabaseName,
	}
}

func createTrafficManager(mainDatabase *p2p_database.DB, configuration *config.Config) *TrafficManager {
	return NewTrafficManager(mainDatabase, configuration.LoggingP2P)
}

func CreateMainDatabaseP2P(conf p2p_database.Config, c *config.Config) (*p2p_database.DB, error) {
	db, err := p2p_database.Connect(context.Background(), conf, c.LoggingP2P)
	if err != nil {
		return nil, errors.Wrap(err, "create main p2p db")
	}
	return db, nil
}

func getNodeID(currentNode routing.LocalNode) livekit2.NodeID {
	return livekit2.NodeID(currentNode.Id)
}

func createKeyProvider(conf *config.Config, contract *p2p_database.EthSmartContract) (auth.KeyProvider, error) {
	return createKeyPublicKeyProvider(conf, contract)
}

func createKeyPublicKeyProvider(conf *config.Config, contract *p2p_database.EthSmartContract) (auth.KeyProviderPublicKey, error) {
	return auth.NewEthKeyProvider(*contract, conf.Ethereum.WalletAddress, conf.Ethereum.WalletPrivateKey), nil
}

func createWebhookNotifier(conf *config.Config, provider auth.KeyProvider) (webhook.Notifier, error) {
	wallet := conf.Ethereum.WalletAddress
	secret := provider.GetSecret(wallet)
	if secret == "" {
		return nil, ErrWebHookMissingAPIKey
	}

	return webhook.NewNotifier(wallet, secret), nil
}

func createRedisClient(conf *config.Config) (redis.UniversalClient, error) {
	if !conf.Redis.IsConfigured() {
		return nil, nil
	}
	return redis2.GetRedisClient(&conf.Redis)
}

func createStore(
	mainDatabase *p2p_database.DB,
	p2pDbConfig p2p_database.Config,
	nodeID livekit2.NodeID,
	participantCounter *ParticipantCounter,
	conf *config.Config,
	nodeProvider *NodeProvider,
) ObjectStore {
	return NewLocalStore(nodeID, participantCounter, mainDatabase, nodeProvider)
}

func getMessageBus(rc redis.UniversalClient) psrpc.MessageBus {
	if rc == nil {
		return psrpc.NewLocalMessageBus()
	}
	return psrpc.NewRedisMessageBus(rc)
}

func getEgressClient(conf *config.Config, nodeID livekit2.NodeID, bus psrpc.MessageBus) (rpc.EgressClient, error) {
	if conf.Egress.UsePsRPC {
		return rpc.NewEgressClient(nodeID, bus)
	}

	return nil, nil
}

func getEgressStore(s ObjectStore) EgressStore {
	return nil
}

func getIngressStore(s ObjectStore) IngressStore {
	return nil
}

func getIngressConfig(conf *config.Config) *config.IngressConfig {
	return &conf.Ingress
}

func createClientConfiguration() clientconfiguration.ClientConfigurationManager {
	return clientconfiguration.NewStaticClientConfigurationManager(clientconfiguration.StaticConfigurations)
}

func getRoomConf(config2 *config.Config) config.RoomConfig {
	return config2.Room
}

func getSignalRelayConfig(config2 *config.Config) config.SignalRelayConfig {
	return config2.SignalRelay
}

func newInProcessTurnServer(conf *config.Config, authHandler turn.AuthHandler) (*turn.Server, error) {
	return NewTurnServer(conf, authHandler, false)
}
