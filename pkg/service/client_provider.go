package service

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"

	p2p_database "github.com/dTelecom/p2p-realtime-database"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ipfs/go-log/v2"
	"github.com/pkg/errors"
)

const (
	databasePrefixClientKey              = "eth_client_"
	defaultTtl                           = time.Hour
	defaultIntervalCheckingExpiredRecord = 5 * time.Minute
)

type ClientProvider struct {
	mainDatabase *p2p_database.DB
	contract     *p2p_database.EthSmartContract
	logger       *log.ZapEventLogger
}

type Client struct {
	Limit  *big.Int `json:"limit"`
	Until  *big.Int `json:"until"`
	Active bool     `json:"active"`
	Key    string   `json:"key"`
}

type rowDatabaseRecord struct {
	Client Client `json:"client"`
	TTL    int64  `json:"ttl"`
}

func NewClientProvider(database *p2p_database.DB, contract *p2p_database.EthSmartContract, logger *log.ZapEventLogger) *ClientProvider {
	provider := &ClientProvider{
		mainDatabase: database,
		contract:     contract,
		logger:       logger,
	}

	provider.startRemovingExpiredRecord()

	return provider
}

func (c *ClientProvider) List(ctx context.Context) ([]Client, error) {
	keys, err := c.mainDatabase.List(ctx)
	if err != nil {
		return []Client{}, errors.Wrap(err, "list keys")
	}

	var clients []Client
	for _, k := range keys {
		if !strings.HasPrefix(k, "/"+databasePrefixClientKey) {
			continue
		}
		clientId := strings.TrimLeft(k, "/"+databasePrefixClientKey)
		client, err := c.ClientByAddress(ctx, clientId)
		if err != nil {
			c.logger.Errorw("key not found "+k, err)
			continue
		}
		clients = append(clients, client)
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
	key := databasePrefixClientKey + address

	row, err := c.mainDatabase.Get(ctx, key)
	if err != nil {
		return Client{}, errors.Wrap(err, "get row")
	}

	var result rowDatabaseRecord
	err = json.Unmarshal([]byte(row), &result)
	if err != nil {
		return Client{}, errors.Wrap(err, "unmarshal record")
	}

	return result.Client, nil
}

func (c *ClientProvider) saveInDatabase(ctx context.Context, address string, client Client) error {
	record := rowDatabaseRecord{
		Client: client,
		TTL:    time.Now().Add(defaultNodeTtl).Unix(),
	}

	marshaled, err := json.Marshal(record)
	if err != nil {
		return errors.Wrap(err, "marshal record")
	}

	err = c.mainDatabase.Set(ctx, databasePrefixClientKey+address, string(marshaled))
	if err != nil {
		return errors.Wrap(err, "database set record")
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

func (c *ClientProvider) startRemovingExpiredRecord() {
	go func() {

		ticker := time.NewTicker(defaultIntervalCheckingExpiredRecord)
		for {
			<-ticker.C
			ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
			defer cancel()

			now := time.Now().Unix()

			keys, err := c.mainDatabase.List(ctx)
			if err != nil {
				c.logger.Errorw("[startRemovingExpiredRecord] error list main databases keys %s\r\n", err)
				continue
			}

			for _, key := range keys {
				key = strings.TrimPrefix(key, "/")
				if !strings.HasPrefix(key, databasePrefixClientKey) {
					continue
				}

				row, err := c.mainDatabase.Get(ctx, key)
				if err != nil {
					c.logger.Errorw("[startRemovingExpiredRecord] get database record with key %s error %s\r\n", key, err)
					continue
				}

				var result rowDatabaseRecord
				err = json.Unmarshal([]byte(row), &result)
				if err != nil {
					c.logger.Errorw("[startRemovingExpiredRecord] unmarshal record with key %s error %s\r\n", key, err)
					continue
				}

				if now > result.TTL {
					err = c.mainDatabase.Remove(ctx, key)
					if err != nil {
						c.logger.Errorw("[startRemovingExpiredRecord] remove expired record with key %s error %s\r\n", key, err)
						continue
					} else {
						c.logger.Infow("[startRemovingExpiredRecord] removed expired record with", "key", key)
					}
				}
			}
		}
	}()
}
