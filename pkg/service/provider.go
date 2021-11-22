package service

import (
	"context"
	"github.com/go-redis/redis/v8"
)

const (
	LIVEKIT_KEYS = "livekit-keys"
)

type RedisBasedKeyProvider struct {
	client *redis.Client
}

func NewRedisBasedKeyProvider(c *redis.Client) *RedisBasedKeyProvider {
	return &RedisBasedKeyProvider{
		client: c,
	}
}

func (p *RedisBasedKeyProvider) GetSecret(key string) string {
	secret, _ := p.client.HGet(context.Background(), LIVEKIT_KEYS, key).Result()
	return secret
}

func (p *RedisBasedKeyProvider) NumKeys() int {
	numKey, _ := p.client.HLen(context.Background(), "livekit-keys").Result()
	return int(numKey)
}
