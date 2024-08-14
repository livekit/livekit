package service

import (
	"context"
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
	AudioBandwidth              = 1_500
	VideoBanwithHigh            = 1500_000
	VideoBanwithMedium          = 750_000
	VideoBanwithLow             = 375_000
	topicTrafficValuesStreaming = "clients_traffic"
)

type trafficMessage struct {
	ValueByApiKey map[livekit.ApiKey]int64 `json:"valueByApiKey"`
	CreatedAt     time.Time                `json:"createdAt"`
}

type trafficValue struct {
	value     int64
	createdAt time.Time
}

func (v *trafficValue) isExpired() bool {
	return time.Now().Sub(v.createdAt).Seconds() >= 10
}

type TrafficManager struct {
	db                    *p2p_database.DB
	lock                  sync.RWMutex
	trafficValuesByApiKey map[livekit.ApiKey]map[string]trafficValue
	logger                *log.ZapEventLogger
}

func NewTrafficManager(db *p2p_database.DB, logger *log.ZapEventLogger) *TrafficManager {
	m := &TrafficManager{
		db:                    db,
		lock:                  sync.RWMutex{},
		trafficValuesByApiKey: make(map[livekit.ApiKey]map[string]trafficValue),
		logger:                logger,
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

	trafficValueByPeerId, ok := m.trafficValuesByApiKey[clientApiKey]
	if !ok {
		return 0
	}

	var value int64
	for peerId, trafficValue := range trafficValueByPeerId {
		if trafficValue.isExpired() {
			delete(m.trafficValuesByApiKey[clientApiKey], peerId)
		} else {
			value += trafficValue.value
		}
	}

	return value
}

func (m *TrafficManager) SetValue(ctx context.Context, valueByApiKey map[livekit.ApiKey]int64) error {
	trafficMsg := trafficMessage{
		ValueByApiKey: valueByApiKey,
		CreatedAt:     time.Now().UTC(),
	}

	m.handleTrafficMessage(trafficMsg, "local")

	jsonMsg, err := json.Marshal(trafficMsg)
	if err != nil {
		return errors.Wrap(err, "marshal traffic message")
	}

	_, err = m.db.Publish(ctx, topicTrafficValuesStreaming, string(jsonMsg))
	if err != nil {
		return errors.Wrap(err, "publish traffic message")
	}

	return nil
}

func (m *TrafficManager) init(ctx context.Context) error {
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.lock.Lock()
				for clientApiKey, trafficsPerPeers := range m.trafficValuesByApiKey {
					for peerId, trafficMessage := range trafficsPerPeers {
						if !trafficMessage.isExpired() {
							continue
						}
						delete(m.trafficValuesByApiKey[clientApiKey], peerId)
					}
				}
				m.lock.Unlock()
			}
		}
	}()

	return m.db.Subscribe(ctx, topicTrafficValuesStreaming, func(event p2p_database.Event) {
		jsonMsg, ok := event.Message.(string)
		if !ok {
			m.logger.Errorw("convert interface to string from message topic traffic values")
			return
		}

		trafficMsg := trafficMessage{}
		err := json.Unmarshal([]byte(jsonMsg), &trafficMsg)
		if err != nil {
			m.logger.Errorw("topic traffic values unmarshal error", err)
			return
		}

		m.handleTrafficMessage(trafficMsg, event.FromPeerId)
	})
}

func (m *TrafficManager) handleTrafficMessage(trafficMsg trafficMessage, fromPeerId string) {
	m.lock.Lock()
	defer m.lock.Unlock()

	for apiKey, value := range trafficMsg.ValueByApiKey {
		trafficValueByPeerId, ok := m.trafficValuesByApiKey[apiKey]
		if !ok {
			trafficValueByPeerId = map[string]trafficValue{}
		}
		trafficValueByPeerId[fromPeerId] = trafficValue{
			value:     value,
			createdAt: trafficMsg.CreatedAt,
		}
		m.trafficValuesByApiKey[apiKey] = trafficValueByPeerId
	}
}
