package utils

import (
	"context"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/karlseguin/ccache/v2"
	"github.com/pkg/errors"
)

const (
	defaultCacheTTL = time.Minute
)

type CachedRedis struct {
	ctx   context.Context
	rc    *redis.Client
	cache *ccache.Cache
}

func NewCachedRedis(ctx context.Context, rc *redis.Client) *CachedRedis {
	return &CachedRedis{
		ctx:   ctx,
		rc:    rc,
		cache: ccache.New(ccache.Configure()),
	}
}

func (r *CachedRedis) CachedHGet(key, hashKey string) (string, error) {
	cacheKey := key + ":" + hashKey
	item := r.cache.Get(cacheKey)
	if item != nil && !item.Expired() {
		return item.Value().(string), nil
	}
	val, err := r.rc.HGet(r.ctx, key, hashKey).Result()
	if err != nil {
		return "", errors.Wrapf(err, "could not hget %s[%s]", key, hashKey)
	}
	r.cache.Set(key, val, defaultCacheTTL)
	return val, nil
}

func (r *CachedRedis) CachedGet(key string) (string, error) {
	item := r.cache.Get(key)
	if item != nil && !item.Expired() {
		return item.Value().(string), nil
	}
	val, err := r.rc.Get(r.ctx, key).Result()
	if err != nil {
		return "", errors.Wrapf(err, "could not get %s", key)
	}

	r.cache.Set(key, val, defaultCacheTTL)
	return val, nil
}

func (r *CachedRedis) ExpireHash(key, hashKey string) {
	r.cache.Delete(key + ":" + hashKey)
}

func (r *CachedRedis) Expire(key string) {
	r.cache.Delete(key)
}
