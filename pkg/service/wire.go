//go:build wireinject
// +build wireinject

package service

import (
	"context"

	p2p_database "github.com/dTelecom/p2p-realtime-database"
	"github.com/google/wire"
	"github.com/inconshreveable/go-vhost"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/egress"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	redisLiveKit "github.com/livekit/protocol/redis"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/protocol/webhook"
	"github.com/livekit/psrpc"
	"github.com/oschwald/geoip2-golang"
	"github.com/pion/turn/v2"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/acme/autocert"

	livekit2 "github.com/livekit/livekit-server"
	"github.com/livekit/livekit-server/pkg/clientconfiguration"
	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/telemetry"
)

func InitializeServer(conf *config.Config, currentNode routing.LocalNode) (*LivekitServer, error) {
	wire.Build(
		getNodeID,
		CreateNodeProvider,
		CreateMainDatabaseP2P,
		createRedisClient,
		createStore,
		wire.Bind(new(ServiceStore), new(ObjectStore)),
		createKeyProvider,
		createKeyPublicKeyProvider,
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
		NewCertManager,
		NewVhostMuxer,
		newTurnAuthHandler,
		newInProcessTurnServer,
		utils.NewDefaultTimedVersionGenerator,
		createClientProvider,
		createGeoIP,
		createRelevantNodesHandler,
		createMainDebugHandler,
		NewLivekitServer,
	)
	return &LivekitServer{}, nil
}

func CreateMainDatabaseP2P(conf *config.Config, nodeProvider *NodeProvider) (*p2p_database.DB, error) {
	p2pConf := p2p_database.Config{
		DisableGater:     false,
		WalletPrivateKey: conf.Solana.WalletPrivateKey,
		PeerListenPort:   conf.P2P.PeerListenPort,
		DatabaseName:     conf.P2P.DatabaseName,
		GetNodes:         nodeProvider.GetNodes,
	}
	adaptedLogger := p2p_database.NewLivekitLoggerAdapter(logger.GetLogger())
	db, err := p2p_database.Connect(context.Background(), p2pConf, adaptedLogger)
	if err != nil {
		return nil, err
	}
	return db, nil
}

func createRelevantNodesHandler(nodeProvider *NodeProvider) *RelevantNodesHandler {
	return NewRelevantNodesHandler(nodeProvider)
}

func createMainDebugHandler(nodeProvider *NodeProvider, clientProvider *ClientProvider, db *p2p_database.DB) *MainDebugHandler {
	return NewMainDebugHandler(nodeProvider, clientProvider, db)
}

func createGeoIP() (*geoip2.Reader, error) {
	return geoip2.FromBytes(livekit2.MixmindDatabase)
}

func CreateNodeProvider(geo *geoip2.Reader, config *config.Config, node routing.LocalNode) *NodeProvider {
	return NewNodeProvider(geo, node, config.Solana)
}

func createClientProvider(config *config.Config) *ClientProvider {
	return NewClientProvider(config.Solana)
}

func getNodeID(currentNode routing.LocalNode) livekit.NodeID {
	return livekit.NodeID(currentNode.Id)
}

func createKeyProvider(conf *config.Config) (auth.KeyProvider, error) {
	return createKeyPublicKeyProvider(conf)
}

func createKeyPublicKeyProvider(conf *config.Config) (auth.KeyProviderPublicKey, error) {
	return auth.NewSolanaKeyProvider(conf.Solana.WalletPrivateKey), nil
}

func createWebhookNotifier(keyProvider auth.KeyProvider, nodeID livekit.NodeID) (webhook.Notifier, error) {
	key := string(nodeID)

	secret := keyProvider.GetSecret(key)
	if secret == "" {
		return nil, ErrWebHookMissingAPIKey
	}

	return webhook.NewNotifier(key, secret), nil
}

func createRedisClient(conf *config.Config) (redis.UniversalClient, error) {
	if !conf.Redis.IsConfigured() {
		return nil, nil
	}
	return redisLiveKit.GetRedisClient(&conf.Redis)
}

func createStore(
	db *p2p_database.DB,
	nodeID livekit.NodeID,
) ObjectStore {
	return NewLocalStore(nodeID, db)
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

func getRoomConf(config *config.Config) config.RoomConfig {
	return config.Room
}

func getSignalRelayConfig(config *config.Config) config.SignalRelayConfig {
	return config.SignalRelay
}

func newInProcessTurnServer(conf *config.Config, authHandler turn.AuthHandler, TLSMuxer *vhost.TLSMuxer, certManager *autocert.Manager) (*turn.Server, error) {
	return NewTurnServer(conf, authHandler, false, TLSMuxer, certManager)
}
