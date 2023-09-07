package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"sync"
	"time"

	p2p_database "github.com/dTelecom/p2p-realtime-database"
	"github.com/ipfs/go-log/v2"
	"github.com/livekit/protocol/livekit"
	"github.com/pkg/errors"
)

const (
	TrafficLimitPerClient       = 5000_000
	AudioBandwidth              = 1500
	VideoBanwith                = 1500_000
	topicTrafficValuesStreaming = "clients_traffic"
)

type TrafficMessage struct {
	ClientApiKey livekit.ApiKey `json:"clientApiKey"`
	Value        int64          `json:"value"`
	CreatedAt    time.Time      `json:"createdAt"`
}

func (m *TrafficMessage) isExpired() bool {
	return time.Now().Sub(m.CreatedAt).Seconds() >= 10
}

type TrafficManager struct {
	db                *p2p_database.DB
	lock              sync.RWMutex
	trafficsPerClient map[livekit.ApiKey]map[string]TrafficMessage
	logger            *log.ZapEventLogger
}

func NewTrafficManager(db *p2p_database.DB, logger *log.ZapEventLogger) *TrafficManager {
	m := &TrafficManager{
		db:                db,
		lock:              sync.RWMutex{},
		trafficsPerClient: make(map[livekit.ApiKey]map[string]TrafficMessage),
		logger:            logger,
	}

	err := m.init(context.Background())
	if err != nil {
		panic(err)
	}

	return m
}

func (m *TrafficManager) GetValue(clientApiKey livekit.ApiKey) int64 {
	m.lock.RLock()
	defer m.lock.RUnlock()

	var value int64

	traffics, ok := m.trafficsPerClient[clientApiKey]
	if !ok {
		return 0
	}

	for peerId, traffic := range traffics {
		if traffic.isExpired() {
			delete(m.trafficsPerClient[clientApiKey], peerId)
		} else {
			value += traffic.Value
		}
	}

	return value
}

func (m *TrafficManager) SetValue(ctx context.Context, clientApiKey livekit.ApiKey, value int64) error {
	trafficMessage := TrafficMessage{
		ClientApiKey: clientApiKey,
		Value:        value,
		CreatedAt:    time.Now().UTC(),
	}

	body, err := json.Marshal(trafficMessage)
	if err != nil {
		return errors.Wrap(err, "marshal traffic message")
	}

	_, err = m.db.Publish(ctx, topicTrafficValuesStreaming, base64.StdEncoding.EncodeToString(body))
	if err != nil {
		return errors.Wrap(err, "publish traffic message")
	}

	return nil
}

func (m *TrafficManager) init(ctx context.Context) error {
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for clientApiKey, trafficsPerPeers := range m.trafficsPerClient {
					for peerId, trafficMessage := range trafficsPerPeers {
						if !trafficMessage.isExpired() {
							continue
						}
						m.lock.Lock()
						delete(m.trafficsPerClient[clientApiKey], peerId)
						m.lock.Unlock()
					}
				}
			}
		}
	}()

	return m.db.Subscribe(ctx, topicTrafficValuesStreaming, func(event p2p_database.Event) {
		b64s, ok := event.Message.(string)
		if !ok {
			m.logger.Errorw("convert interface to string from message topic traffic values")
			return
		}

		bytes, err := base64.StdEncoding.DecodeString(b64s)
		if err != nil {
			m.logger.Errorw("decode base64", err)
			return
		}

		trafficMessage := TrafficMessage{}
		err = json.Unmarshal(bytes, &trafficMessage)
		if err != nil {
			m.logger.Errorw("topic traffic values unmarshal error", err)
			return
		}

		peerId := event.FromPeerId
		if err != nil {
			m.logger.Errorw("form peer id from string ", peerId, err)
			return
		}

		m.lock.Lock()
		m.trafficsPerClient[trafficMessage.ClientApiKey] = map[string]TrafficMessage{
			peerId: trafficMessage,
		}
		m.lock.Unlock()
	})
}
