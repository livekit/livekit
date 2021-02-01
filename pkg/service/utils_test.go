package service_test

import (
	"github.com/go-redis/redis/v8"
)

func redisClient() *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr: "localhost:6379",
	})
}
