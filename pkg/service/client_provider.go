package service

import (
	"context"
	"encoding/binary"
	"fmt"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/gagliardetto/solana-go/rpc/ws"
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
	RegistryEntrySize = 8 + 32 + 32 + 8 + 4 // discriminator + parent + registered + until + limit
	defaultClientTtl  = 600 * time.Second
)

// RegistryClient represents a client for interacting with the registry program
type RegistryClient struct {
	programID solana.PublicKey
	client    *rpc.Client
	wsClient  *ws.Client
}

// RegistryEntry represents an entry in the registry
type RegistryEntry struct {
	Parent    solana.PublicKey
	Registred solana.PublicKey
	Until     int64
	Limit     uint32
}

type ClientProvider struct {
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

	client, err := newRegistryClient(c.NetworkHostHTTP, c.NetworkHostWS, c.ContractAddress)
	if err != nil {
		return Client{}, err
	}
	entry, err := client.GetFromRegistry(ctx, authority, "clients", account)
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

func newRegistryClient(rpcEndpoint string, wsEndpoint string, programID string) (*RegistryClient, error) {
	client := rpc.New(rpcEndpoint)

	wsClient, err := ws.Connect(context.Background(), wsEndpoint)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to websocket: %v", err)
	}

	programPubkey, err := solana.PublicKeyFromBase58(programID)
	if err != nil {
		return nil, fmt.Errorf("invalid program ID: %v", err)
	}

	return &RegistryClient{
		programID: programPubkey,
		client:    client,
		wsClient:  wsClient,
	}, nil
}

// GetFromRegistry retrieves an entry from the registry
func (c *RegistryClient) GetFromRegistry(ctx context.Context, registryAuthority solana.PublicKey, registryName string, accountToCheck solana.PublicKey) (*RegistryEntry, error) {
	// Find the registry PDA
	registryPDA, _, err := findRegistryPDA(c.programID, registryAuthority, registryName)
	if err != nil {
		return nil, fmt.Errorf("failed to find registry PDA: %v", err)
	}

	return getRegistryEntry(ctx, c.client, c.programID, registryPDA, accountToCheck)
}

// findRegistryPDA finds the PDA for a registry with the given name
func findRegistryPDA(programID solana.PublicKey, authority solana.PublicKey, name string) (solana.PublicKey, uint8, error) {
	return solana.FindProgramAddress(
		[][]byte{
			authority.Bytes(),
			[]byte(name),
		},
		programID,
	)
}

// getRegistryEntry retrieves a registry entry account data
func getRegistryEntry(
	ctx context.Context,
	client *rpc.Client,
	programID solana.PublicKey,
	registry solana.PublicKey,
	accountToCheck solana.PublicKey,
) (*RegistryEntry, error) {
	entryPDA, _, err := findRegistryEntryPDA(programID, accountToCheck, registry)
	if err != nil {
		return nil, fmt.Errorf("failed to find entry PDA: %v", err)
	}

	// Get the account info
	accountInfo, err := client.GetAccountInfo(ctx, entryPDA)
	if err != nil {
		return nil, fmt.Errorf("failed to get account info: %v", err)
	}

	if accountInfo == nil || len(accountInfo.Value.Data.GetBinary()) == 0 {
		return nil, nil // Account doesn't exist
	}

	// Parse the account data
	data := accountInfo.Value.Data.GetBinary()
	if len(data) != RegistryEntrySize {
		return nil, fmt.Errorf("invalid account data size: expected %d, got %d", RegistryEntrySize, len(data))
	}

	// Skip the 8-byte discriminator
	data = data[8:]

	entry := &RegistryEntry{
		Parent:    solana.PublicKeyFromBytes(data[:32]),
		Registred: solana.PublicKeyFromBytes(data[32:64]),
		Until:     int64(binary.LittleEndian.Uint64(data[64:72])),
		Limit:     binary.LittleEndian.Uint32(data[72:76]),
	}

	return entry, nil
}

// findRegistryEntryPDA finds the PDA for a registry entry
func findRegistryEntryPDA(programID solana.PublicKey, accountToAdd solana.PublicKey, registry solana.PublicKey) (solana.PublicKey, uint8, error) {
	return solana.FindProgramAddress(
		[][]byte{
			accountToAdd.Bytes(),
			registry.Bytes(),
		},
		programID,
	)
}
