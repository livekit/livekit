package service

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	p2p_database "github.com/dTelecom/p2p-realtime-database"
	"github.com/ipfs/go-log/v2"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/pkg/errors"
)

const (
	trafficLimitPerClient       = 5000000
	topicTrafficValuesStreaming = "clients_traffic"
)

type TrafficMessage struct {
	ClientApiKey string    `json:"clientApiKey"`
	Value        int       `json:"value"`
	CreatedAt    time.Time `json:"createdAt"`
}

func (m *TrafficMessage) isExpired() bool {
	return time.Now().Sub(m.CreatedAt).Seconds() >= 10
}

type TrafficManager struct {
	db                *p2p_database.DB
	clientProvider    *ClientProvider
	lock              sync.RWMutex
	trafficsPerClient map[string]map[peer.ID]TrafficMessage
	logger            *log.ZapEventLogger
}

func NewTrafficManager(db *p2p_database.DB, clientProvider *ClientProvider, logger *log.ZapEventLogger) *TrafficManager {
	m := &TrafficManager{
		db:                db,
		clientProvider:    clientProvider,
		lock:              sync.RWMutex{},
		trafficsPerClient: make(map[string]map[peer.ID]TrafficMessage),
		logger:            logger,
	}

	err := m.init(context.Background())
	if err != nil {
		panic(err)
	}

	return m
}

func (m *TrafficManager) GetValue(ctx context.Context, clientApiKey string) (int, error) {
	m.lock.RLock()
	defer m.lock.RUnlock()

	var value int

	traffics, ok := m.trafficsPerClient[clientApiKey]
	if !ok {
		return 0, nil
	}

	for peerId, traffic := range traffics {
		if traffic.isExpired() {
			delete(m.trafficsPerClient[clientApiKey], peerId)
		} else {
			value += traffic.Value
		}
	}

	return value, nil
}

func (m *TrafficManager) SetValue(ctx context.Context, clientApiKey string, value int) error {
	trafficMessage := TrafficMessage{
		ClientApiKey: clientApiKey,
		Value:        value,
		CreatedAt:    time.Now().UTC(),
	}

	body, err := json.Marshal(trafficMessage)
	if err != nil {
		return errors.Wrap(err, "marshal traffic message")
	}

	_, err = m.db.Publish(ctx, topicTrafficValuesStreaming, body)
	if err != nil {
		return errors.Wrap(err, "publish traffic message")
	}

	return nil
}

func (m *TrafficManager) GetLimit(ctx context.Context, clientApiKey string) (int, error) {
	client, err := m.clientProvider.ClientByAddress(ctx, clientApiKey)
	if err != nil {
		return 0, errors.Wrap(err, "client by address")
	}

	clientLimit := int(client.Limit.Int64())

	return clientLimit * trafficLimitPerClient, nil
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
		bytes, ok := event.Message.([]byte)
		if !ok {
			m.logger.Errorw("convert interface to bytes from message topic traffic values")
			return
		}

		trafficMessage := TrafficMessage{}
		err := json.Unmarshal(bytes, &trafficMessage)
		if err != nil {
			m.logger.Errorw("topic traffic values unmarshal error", err)
			return
		}

		peerId, err := peer.IDFromString(event.FromPeerId)
		if err != nil {
			m.logger.Errorw("form peer id from string ", peerId, err)
			return
		}

		m.lock.Lock()
		m.trafficsPerClient[trafficMessage.ClientApiKey] = map[peer.ID]TrafficMessage{
			peerId: trafficMessage,
		}
		m.lock.Unlock()
	})
}
