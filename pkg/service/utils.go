package service

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"regexp"

	"github.com/go-redis/redis/v8"
	"github.com/google/wire"
	"github.com/livekit/livekit-server/pkg/routing/selector"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/logger"
	livekit "github.com/livekit/protocol/proto"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/protocol/webhook"
	"github.com/pkg/errors"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
)

var ServiceSet = wire.NewSet(
	createRedisClient,
	createMessageBus,
	createRouter,
	createStore,
	CreateKeyProvider,
	CreateWebhookNotifier,
	CreateNodeSelector,
	NewRecordingService,
	NewRoomAllocator,
	NewRoomService,
	NewRTCService,
	NewLivekitServer,
	NewLocalRoomManager,
	NewTurnServer,
	config.GetAudioConfig,
	wire.Bind(new(RoomManager), new(*LocalRoomManager)),
	wire.Bind(new(livekit.RoomService), new(*RoomService)),
)

func CreateKeyProvider(conf *config.Config) (auth.KeyProvider, error) {
	// prefer keyfile if set
	if conf.KeyFile != "" {
		if st, err := os.Stat(conf.KeyFile); err != nil {
			return nil, err
		} else if st.Mode().Perm() != 0600 {
			return nil, fmt.Errorf("key file must have permission set to 600")
		}
		f, err := os.Open(conf.KeyFile)
		if err != nil {
			return nil, err
		}
		defer func() {
			_ = f.Close()
		}()
		return auth.NewFileBasedKeyProviderFromReader(f)
	}

	if len(conf.Keys) == 0 {
		return nil, errors.New("one of key-file or keys must be provided in order to support a secure installation")
	}

	return auth.NewFileBasedKeyProviderFromMap(conf.Keys), nil
}

func CreateWebhookNotifier(conf *config.Config, provider auth.KeyProvider) (webhook.Notifier, error) {
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

func CreateNodeSelector(conf *config.Config) (routing.NodeSelector, error) {
	kind := conf.NodeSelector.Kind
	if kind == "" {
		kind = "random"
	}
	switch kind {
	case "sysload":
		return &selector.SystemLoadSelector{
			SysloadLimit: conf.NodeSelector.SysloadLimit,
		}, nil
	case "regionaware":
		s, err := selector.NewRegionAwareSelector(conf.Region, conf.NodeSelector.Regions)
		if err != nil {
			return nil, err
		}
		s.SysloadLimit = conf.NodeSelector.SysloadLimit
		return s, nil
	case "random":
		return &selector.RandomSelector{}, nil
	default:
		return nil, ErrUnsupportedSelector
	}
}

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

func createMessageBus(rc *redis.Client) utils.MessageBus {
	if rc == nil {
		return nil
	}
	return utils.NewRedisMessageBus(rc)
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
