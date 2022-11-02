package service

import (
	"strings"
	"sync"
	"time"

	"github.com/jellydator/ttlcache/v2"

	"github.com/livekit/protocol/livekit"
)

const (
	KeySeparator = "!_!"
)

type LocalDataStreamStore struct {
	sync.RWMutex
	ttl   time.Duration
	Cache *ttlcache.Cache
}

func NewLocalDataStreamStore(ttl time.Duration) DataStreamStore {
	return &LocalDataStreamStore{
		Cache: ttlcache.NewCache(),
		ttl:   ttl,
	}
}

func generateKeyName(bucket string, key string) string {
	return bucket + KeySeparator + key
}

func getBucketNameFromKey(key string) string {
	return strings.Split(key, KeySeparator)[0]
}

func (ds *LocalDataStreamStore) CreateBucket(bucket string, ttl time.Duration) error {
	// no op
	return nil
}

func (ds *LocalDataStreamStore) DeleteBucket(bucket string) error {
	ds.Lock()
	defer ds.Unlock()
	for _, key := range ds.Cache.GetKeys() {
		if bucket == getBucketNameFromKey(key) {
			ds.Cache.Remove(key)
		}
	}
	return nil
}

func (ds *LocalDataStreamStore) Get(bucket string, key string) (*livekit.DataPacket_Stream, error) {
	ds.RLock()
	defer ds.RUnlock()
	v, err := ds.Cache.Get(generateKeyName(bucket, key))
	if err != nil {
		return nil, err
	}
	s := v.(livekit.DataPacket_Stream)
	return &s, nil
}

func (ds *LocalDataStreamStore) GetAll(bucket string) ([]*livekit.DataPacket_Stream, error) {
	var rsp []*livekit.DataPacket_Stream
	ds.RLock()
	defer ds.RUnlock()
	for _, key := range ds.Cache.GetKeys() {
		//logger.Infow("==========GET FOUND", "key", key, "bucket", bucket)
		if bucket == getBucketNameFromKey(key) {
			//	logger.Infow("==========GOT MATCHED", "key", key, "bucket", bucket)
			d, err := ds.Cache.Get(key)
			if err != nil {
				continue
			}
			s := d.(livekit.DataPacket_Stream)
			rsp = append(rsp, &s)
		}
	}
	return rsp, nil
}

func (ds *LocalDataStreamStore) Put(bucket string, key string, value *livekit.DataPacket_Stream) error {
	ds.Lock()
	defer ds.Unlock()
	//	logger.Infow("==========PUT", "key", value.Stream.GetKey(), "name", value.Stream.GetName(),
	//		"bucket", bucket)
	return ds.Cache.SetWithTTL(generateKeyName(bucket, key), *value, ds.ttl)
}
