//go:build wireinject
// +build wireinject

package service

import (
	"context"
	"fmt"
	"os"

	"github.com/go-redis/redis/v8"
	"github.com/google/wire"
	"github.com/pkg/errors"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/protocol/webhook"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/telemetry"
)

func InitializeServer(conf *config.Config, currentNode routing.LocalNode) (*LivekitServer, error) {
	wire.Build(
		createRedisClient,
		createMessageBus,
		createStore,
		wire.Bind(new(RORoomStore), new(RoomStore)),
		createKeyProvider,
		createWebhookNotifier,
		routing.CreateRouter,
		wire.Bind(new(routing.MessageRouter), new(routing.Router)),
		telemetry.NewTelemetryService,
		NewRecordingService,
		NewRoomAllocator,
		NewRoomService,
		NewRTCService,
		NewLocalRoomManager,
		newTurnAuthHandler,
		NewTurnServer,
		NewLivekitServer,
	)
	return &LivekitServer{}, nil
}

func InitializeRouter(conf *config.Config, currentNode routing.LocalNode) (routing.Router, error) {
	wire.Build(
		createRedisClient,
		routing.CreateRouter,
	)

	return nil, nil
}

func createKeyProvider(conf *config.Config) (auth.KeyProvider, error) {
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

func createRedisClient(conf *config.Config) (*redis.Client, error) {
	if !conf.HasRedis() {
		return nil, nil
	}

	logger.Infow("using multi-node routing via redis", "addr", conf.Redis.Address)
	if conf.Redis.UseSentinel {
		rc := redis.NewClient(&redis.FailoverOptions{
			SentinelAddrs:    conf.Redis.SentinelAddresses,
			SentinelUsername: conf.Redis.SentinelUsername,
			SentinelPassword: conf.Redis.SentinelPassword,
			MasterName:       conf.Redis.MasterName,
			Username:         conf.Redis.Username,
			Password:         conf.Redis.Password,
			DB:               conf.Redis.DB,
		})
	} else {
		rc := redis.NewClient(&redis.Options{
			Addr:     conf.Redis.Address,
			Username: conf.Redis.Username,
			Password: conf.Redis.Password,
			DB:       conf.Redis.DB,
		})
	}
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

func createStore(rc *redis.Client) RoomStore {
	if rc != nil {
		return NewRedisRoomStore(rc)
	}
	return NewLocalRoomStore()
}
