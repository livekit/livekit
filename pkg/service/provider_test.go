package service

import (
	"context"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"testing"
)

func redisClient() *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr: "localhost:6379",
	})
}

func TestNewRedisBasedKeyProvider(t *testing.T) {
	ctx := context.TODO()
	client := redisClient()

	hashKey := "test-key"
	apiKey := "APIBPMGhNnNKPKZ"
	apiSecret := "cYrrbTnN0arXbfxrWGsoKC3kwcVgrZhP9aNMEkJ5RcT"

	client.HSet(ctx, hashKey, apiKey, apiSecret)

	provider := NewRedisBasedKeyProvider(client, hashKey)
	assert.Equal(t, apiSecret, provider.GetSecret(apiKey))
}
