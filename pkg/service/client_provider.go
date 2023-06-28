package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"strings"
	"time"

	p2p_database "github.com/dTelecom/p2p-realtime-database"
	"github.com/ethereum/go-ethereum/common"
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
}

type Client struct {
	Limit  *big.Int `json:"limit"`
	Until  *big.Int `json:"until"`
	Active bool     `json:"active"`
	Key    string   `json:"key"`
}

type rowDatabaseRecord struct {
	Client Client    `json:"client"`
	TTL    time.Time `json:"ttl"`
}

func NewClientProvider(database *p2p_database.DB, contract *p2p_database.EthSmartContract) *ClientProvider {
	provider := &ClientProvider{
		mainDatabase: database,
		contract:     contract,
	}

	provider.startRemovingExpiredRecord()

	return provider
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
	key := databasePrefixClientKey + "_" + address

	row, err := c.mainDatabase.Get(ctx, key)
	if err != nil {
		return Client{}, errors.Wrap(err, "get row")
	}

	var result rowDatabaseRecord
	err = json.Unmarshal([]byte(row), &result)
	if err != nil {
		return Client{}, errors.Wrap(err, "unmarshal record")
	}

	if result.TTL.Before(time.Now()) {
		err = c.mainDatabase.Remove(ctx, key)
		if err != nil {
			return Client{}, errors.Wrap(err, "remove expired record")
		}
		return Client{}, errors.New("client expired")
	}

	return result.Client, nil
}

func (c *ClientProvider) saveInDatabase(ctx context.Context, address string, client Client) error {
	record := rowDatabaseRecord{
		Client: client,
		TTL:    time.Now().Add(defaultTtl),
	}

	marshaled, err := json.Marshal(record)
	if err != nil {
		return errors.Wrap(err, "marshal record")
	}

	err = c.mainDatabase.Set(ctx, databasePrefixClientKey+"_"+address, string(marshaled))
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
		ctx := context.Background()

		ticker := time.NewTicker(defaultIntervalCheckingExpiredRecord)
		for {
			keys, err := c.mainDatabase.List(ctx)
			if err != nil {
				log.Printf("[startRemovingExpiredRecord] error list main databases keys %s\r\n", err)
				return
			}

			for _, key := range keys {
				key = strings.TrimPrefix("/", key)
				if !strings.HasPrefix(key, databasePrefixClientKey) {
					continue
				}

				row, err := c.mainDatabase.Get(ctx, key)
				if err != nil {
					log.Printf("[startRemovingExpiredRecord] get database record with key %s error %s\r\n", key, err)
					continue
				}

				var result rowDatabaseRecord
				err = json.Unmarshal([]byte(row), &result)
				if err != nil {
					log.Printf("[startRemovingExpiredRecord] unmarshal record with key %s error %s\r\n", key, err)
					continue
				}

				if result.TTL.Before(time.Now()) {
					err = c.mainDatabase.Remove(ctx, key)
					if err != nil {
						log.Printf("[startRemovingExpiredRecord] remove expired record with key %s error %s\r\n", key, err)
						continue
					}
				}
			}

			<-ticker.C
		}
	}()
}
