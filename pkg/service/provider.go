package service

import (
	"context"
	"github.com/go-redis/redis/v8"
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
	secret, _ := p.client.Get(context.Background(), key).Result()
	return secret
}

func (p *RedisBasedKeyProvider) NumKeys() int {
	return 1
}
