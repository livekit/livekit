package service

import (
	"context"
	"net/http"
	"regexp"

	"github.com/go-redis/redis/v8"
	"github.com/google/wire"
	"github.com/livekit/protocol/auth"
	livekit "github.com/livekit/protocol/proto"
	"github.com/livekit/protocol/webhook"
	"github.com/pkg/errors"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/routing"
)

var ServiceSet = wire.NewSet(
	createRedisClient,
	createRouter,
	createStore,
	createWebhookNotifier,
	nodeSelectorFromConfig,
	NewRecordingService,
	NewRoomService,
	NewRTCService,
	NewLivekitServer,
	NewLocalRoomManager,
	NewTurnServer,
	config.GetAudioConfig,
	wire.Bind(new(RoomManager), new(*LocalRoomManager)),
	wire.Bind(new(livekit.RecordingService), new(*RecordingService)),
	wire.Bind(new(livekit.RoomService), new(*RoomService)),
)

func createRedisClient(conf *config.Config) (*redis.Client, error) {
	if !conf.HasRedis() {
		return nil, nil
	}

	logger.Infow("using multi-node routing via redis", "addr", conf.Redis.Address)
	rc := redis.NewClient(&redis.Options{
		Addr:     conf.Redis.Address,
		Username: conf.Redis.Username,
		Password: conf.Redis.Password,
		DB:       conf.Redis.DB,
	})
	if err := rc.Ping(context.Background()).Err(); err != nil {
		err = errors.Wrap(err, "unable to connect to redis")
		return nil, err
	}

	return rc, nil
}

func createRouter(rc *redis.Client, node routing.LocalNode) routing.Router {
	if rc != nil {
		return routing.NewRedisRouter(node, rc)
	}

	// local routing and store
	logger.Infow("using single-node routing")
	return routing.NewLocalRouter(node)
}

func createStore(rc *redis.Client) RoomStore {
	if rc != nil {
		return NewRedisRoomStore(rc)
	}
	return NewLocalRoomStore()
}

func createWebhookNotifier(conf *config.Config, provider auth.KeyProvider) (*webhook.Notifier, error) {
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

func nodeSelectorFromConfig(conf *config.Config) routing.NodeSelector {
	switch conf.NodeSelector.Kind {
	case "sysload":
		return &routing.SystemLoadSelector{
			SysloadLimit: conf.NodeSelector.SysloadLimit,
		}
	default:
		return &routing.RandomSelector{}
	}
}

func handleError(w http.ResponseWriter, status int, msg string) {
	// GetLogger already with extra depth 1
	logger.GetLogger().V(1).Info("error handling request", "error", msg, "status", status)
	w.WriteHeader(status)
	_, _ = w.Write([]byte(msg))
}

func boolValue(s string) bool {
	return s == "1" || s == "true"
}

func IsValidDomain(domain string) bool {
	domainRegexp := regexp.MustCompile(`^(?i)[a-z0-9-]+(\.[a-z0-9-]+)+\.?$`)
	return domainRegexp.MatchString(domain)
}

func permissionFromGrant(claim *auth.VideoGrant) *livekit.ParticipantPermission {
	p := &livekit.ParticipantPermission{
		CanSubscribe:   true,
		CanPublish:     true,
		CanPublishData: true,
	}
	if claim.CanPublish != nil {
		p.CanPublish = *claim.CanPublish
	}
	if claim.CanSubscribe != nil {
		p.CanSubscribe = *claim.CanSubscribe
	}
	if claim.CanPublishData != nil {
		p.CanPublishData = *claim.CanPublishData
	}
	return p
}
