package service

import (
	"context"
	"github.com/go-redis/redis/v8"
)

// will remove
const (
	LIVEKIT_KEYS = "livekit-keys"
)

type RedisBasedKeyProvider struct {
	client *redis.Client
	hashKey string
}

func NewRedisBasedKeyProvider(c *redis.Client, hashKey string) *RedisBasedKeyProvider {
	return &RedisBasedKeyProvider{
		client: c,
		hashKey: hashKey,
	}
}

func (p *RedisBasedKeyProvider) GetSecret(key string) string {
	secret, _ := p.client.HGet(context.Background(), p.hashKey, key).Result()
	return secret
}

func (p *RedisBasedKeyProvider) NumKeys() int {
	numKey, _ := p.client.HLen(context.Background(), p.hashKey).Result()
	return int(numKey)
}
