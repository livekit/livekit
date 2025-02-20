package service

import (
	"context"
	"fmt"
	"github.com/gagliardetto/solana-go"
	"github.com/livekit/livekit-server/pkg/config"
	"sync"
	"time"
)

type clientMessage struct {
	Client Client
	TTL    int64
}

func (v *clientMessage) isExpired() bool {
	return time.Now().Unix() >= v.TTL
}

const (
	defaultClientTtl = 600 * time.Second
)

type ClientProvider struct {
	WalletPrivateKey  string
	ContractAddress   string
	NetworkHostHTTP   string
	NetworkHostWS     string
	RegistryAuthority string
	lock              sync.RWMutex
	clientValues      map[string]clientMessage
}

type Client struct {
	Limit int
	Until int64
	Key   string
}

func NewClientProvider(conf config.SolanaConfig) *ClientProvider {
	provider := &ClientProvider{
		WalletPrivateKey:  conf.WalletPrivateKey,
		ContractAddress:   conf.ContractAddress,
		NetworkHostHTTP:   conf.NetworkHostHTTP,
		NetworkHostWS:     conf.NetworkHostWS,
		RegistryAuthority: conf.RegistryAuthority,
		lock:              sync.RWMutex{},
		clientValues:      make(map[string]clientMessage),
	}
	return provider
}

func (c *ClientProvider) ClientByAddress(ctx context.Context, address string) (Client, error) {
	cached, err := c.getFromCache(address)
	if err == nil {
		if cached.isExpired() == false {
			return cached.Client, nil
		}
	}

	account, err := solana.PublicKeyFromBase58(address)
	if err != nil {
		return Client{}, err
	}

	authority, err := solana.PublicKeyFromBase58(c.RegistryAuthority)
	if err != nil {
		return Client{}, err
	}

	client, err := NewRegistryClient(c.NetworkHostHTTP, c.NetworkHostWS, c.ContractAddress, c.WalletPrivateKey)
	if err != nil {
		return Client{}, err
	}
	entry, err := client.GetClientFromRegistry(ctx, authority, "clients", account)
	if err != nil {
		return Client{}, err
	}

	cl := Client{
		Limit: int(entry.Limit),
		Key:   address,
		Until: entry.Until,
	}

	msg := clientMessage{
		Client: cl,
		TTL:    time.Now().Add(defaultClientTtl).Unix(),
	}

	c.lock.Lock()
	defer c.lock.Unlock()
	c.clientValues[address] = msg

	return cl, nil
}

func (c *ClientProvider) getFromCache(address string) (clientMessage, error) {
	c.lock.Lock()
	defer c.lock.Unlock()
	cached, ok := c.clientValues[address]
	if !ok {
		return clientMessage{}, fmt.Errorf("not in cache: %v", address)
	}
	return cached, nil
}

func (c *ClientProvider) List(ctx context.Context) (map[string]Client, error) {
	c.lock.Lock()
	defer c.lock.Unlock()

	clients := make(map[string]Client)
	for k, v := range c.clientValues {
		if v.isExpired() == false {
			clients[k] = v.Client
		}
	}

	return clients, nil
}
