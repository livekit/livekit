package service

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"sync"
	"time"

	p2p_database "github.com/dTelecom/p2p-realtime-database"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ipfs/go-log/v2"
	"github.com/pkg/errors"
)

const (
	defaultClientTtl           = time.Hour
	topicClientValuesStreaming = "clients_values"
)

type ClientProvider struct {
	mainDatabase *p2p_database.DB
	contract     *p2p_database.EthSmartContract
	logger       *log.ZapEventLogger
	lock         sync.RWMutex
	clientValues map[string]clientMessage
}

type Client struct {
	Limit  *big.Int `json:"limit"`
	Until  *big.Int `json:"until"`
	Active bool     `json:"active"`
	Key    string   `json:"key"`
}

type clientMessage struct {
	Client  Client `json:"client"`
	Address string `json:"address"`
	TTL     int64  `json:"ttl"`
}

func (v *clientMessage) isExpired() bool {
	return time.Now().Unix() >= v.TTL
}

func NewClientProvider(mainDatabase *p2p_database.DB, contract *p2p_database.EthSmartContract, logger *log.ZapEventLogger) *ClientProvider {
	provider := &ClientProvider{
		mainDatabase: mainDatabase,
		contract:     contract,
		logger:       logger,
		lock:         sync.RWMutex{},
		clientValues: make(map[string]clientMessage),
	}

	err := mainDatabase.Subscribe(context.Background(), topicClientValuesStreaming, func(event p2p_database.Event) {
		jsonMsg, ok := event.Message.(string)
		if !ok {
			logger.Errorw("convert interface to string from message topic client values")
			return
		}

		clientMsg := clientMessage{}
		err := json.Unmarshal([]byte(jsonMsg), &clientMsg)
		if err != nil {
			logger.Errorw("topic client values unmarshal error", err)
			return
		}

		provider.handleClientMessage(clientMsg)
	})

	if err != nil {
		logger.Errorw("topic client values subscribe error", err)
	}

	return provider
}

func (c *ClientProvider) handleClientMessage(clientMsg clientMessage) {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.clientValues[clientMsg.Address] = clientMsg
}

func (c *ClientProvider) List(ctx context.Context) (map[string]Client, error) {
	c.lock.Lock()
	defer c.lock.Unlock()

	clients := make(map[string]Client)
	for _, v := range c.clientValues {
		if v.isExpired() == false {
			clients[v.Address] = v.Client
		}
	}

	return clients, nil
}

func (c *ClientProvider) ClientByAddress(ctx context.Context, address string) (Client, error) {
	client, err := c.getFromDatabase(ctx, address)
	if err != nil {
		client, errContract := c.getFromContract(address)
		if errContract != nil {
			return Client{}, fmt.Errorf("get from contract error: %w, get from db error: %s", errContract, err)
		}
		err = c.saveInDatabase(ctx, address, client)
		if err != nil {
			return Client{}, fmt.Errorf("save client in db: %w", err)
		}
		return client, nil
	}
	return client, nil
}

func (c *ClientProvider) getFromDatabase(ctx context.Context, address string) (Client, error) {
	c.lock.Lock()
	defer c.lock.Unlock()

	clientMessage, ok := c.clientValues[address]
	if !ok {
		return Client{}, fmt.Errorf("no data in db: %w", address)
	}
	return clientMessage.Client, nil
}

func (c *ClientProvider) saveInDatabase(ctx context.Context, address string, client Client) error {
	record := clientMessage{
		Client:  client,
		TTL:     time.Now().Add(defaultClientTtl).Unix(),
		Address: address,
	}

	c.handleClientMessage(record)

	marshaled, err := json.Marshal(record)
	if err != nil {
		return errors.Wrap(err, "marshal client")
	}

	_, err = c.mainDatabase.Publish(ctx, topicClientValuesStreaming, string(marshaled))
	if err != nil {
		return errors.Wrap(err, "publish client message")
	}

	return nil
}

func (c *ClientProvider) getFromContract(address string) (Client, error) {
	client, err := c.contract.GetEthereumClient().ClientByAddress(nil, common.HexToAddress(address))

	if err != nil {
		return Client{}, errors.Wrap(err, "client by address")
	}

	return Client{
		Limit:  client.Limit,
		Until:  client.Until,
		Active: client.Active,
		Key:    client.Key,
	}, nil
}
