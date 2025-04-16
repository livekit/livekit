package service

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	confirm "github.com/gagliardetto/solana-go/rpc/sendAndConfirmTransaction"
	"github.com/gagliardetto/solana-go/rpc/ws"
)

const (
	ClientEntrySize = 8 + 32 + 32 + 8 + 4           // discriminator + parent + registered + until + limit
	NodeEntrySize   = 8 + 32 + 32 + 4 + 253 + 4 + 1 // discriminator + parent + registered + domain length + domain + online + active
)

// Anchor instruction discriminators
var (
	UpdateNodeOnlineDiscriminator = []byte{35, 22, 232, 250, 60, 30, 62, 83}
)

// RegistryClient represents a client for interacting with the registry program
type RegistryClient struct {
	programID  solana.PublicKey
	client     *rpc.Client
	wsEndpoint string
	signer     solana.PrivateKey
}

// ClientEntry represents a client entry in the registry
type ClientEntry struct {
	Parent    solana.PublicKey
	Registred solana.PublicKey
	Until     int64
	Limit     uint32
}

// NodeEntry represents a node entry in the registry
type NodeEntry struct {
	Parent    solana.PublicKey
	Registred solana.PublicKey
	Domain    string
	Online    int32
	Active    bool
}

// NewRegistryClient creates a new instance of the registry client
func NewRegistryClient(rpcEndpoint string, wsEndpoint string, programID string, privateKey string) (*RegistryClient, error) {
	client := rpc.New(rpcEndpoint)

	programPubkey, err := solana.PublicKeyFromBase58(programID)
	if err != nil {
		return nil, fmt.Errorf("invalid program ID: %v", err)
	}

	privateKeyBytes, err := solana.PrivateKeyFromBase58(privateKey)
	if err != nil {
		return nil, fmt.Errorf("invalid private key: %v", err)
	}

	return &RegistryClient{
		programID:  programPubkey,
		client:     client,
		wsEndpoint: wsEndpoint,
		signer:     privateKeyBytes,
	}, nil
}

// GetClientFromRegistry retrieves a client entry from the registry
func (c *RegistryClient) GetClientFromRegistry(ctx context.Context, authority solana.PublicKey, registryName string, accountToCheck solana.PublicKey) (*ClientEntry, error) {
	// Find the registry PDA
	registryPDA, _, err := findRegistryPDA(c.programID, authority, registryName)
	if err != nil {
		return nil, fmt.Errorf("failed to find registry PDA: %v", err)
	}

	return getClientEntry(ctx, c.client, c.programID, registryPDA, accountToCheck)
}

// GetNodeFromRegistry retrieves a node entry from the registry
func (c *RegistryClient) GetNodeFromRegistry(ctx context.Context, authority solana.PublicKey, registryName string, accountToCheck solana.PublicKey) (*NodeEntry, error) {
	// Find the registry PDA
	registryPDA, _, err := findRegistryPDA(c.programID, authority, registryName)
	if err != nil {
		return nil, fmt.Errorf("failed to find registry PDA: %v", err)
	}

	return getNodeEntry(ctx, c.client, c.programID, registryPDA, accountToCheck)
}

// ListNodesInRegistry retrieves all node entries in the given registry
func (c *RegistryClient) ListNodesInRegistry(ctx context.Context, authority solana.PublicKey, registryName string) ([]*NodeEntry, error) {
	// Find the registry PDA
	registryPDA, _, err := findRegistryPDA(c.programID, authority, registryName)
	if err != nil {
		return nil, fmt.Errorf("failed to find registry PDA: %v", err)
	}

	// Get all program accounts of type NodeEntry
	filters := []rpc.RPCFilter{
		{
			Memcmp: &rpc.RPCFilterMemcmp{
				Offset: 8, // Skip discriminator
				Bytes:  registryPDA.Bytes(),
			},
		},
		{
			DataSize: NodeEntrySize,
		},
	}

	accounts, err := c.client.GetProgramAccountsWithOpts(
		ctx,
		c.programID,
		&rpc.GetProgramAccountsOpts{
			Filters:    filters,
			Commitment: rpc.CommitmentFinalized,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get program accounts: %v", err)
	}

	entries := make([]*NodeEntry, 0, len(accounts))
	for _, acc := range accounts {
		data := acc.Account.Data.GetBinary()
		if len(data) != NodeEntrySize {
			continue
		}

		// Skip the 8-byte discriminator
		data = data[8:]

		// Read domain string length (4 bytes)
		domainLen := binary.LittleEndian.Uint32(data[64:68])

		entry := &NodeEntry{
			Parent:    solana.PublicKeyFromBytes(data[:32]),
			Registred: solana.PublicKeyFromBytes(data[32:64]),
			Domain:    string(data[68 : 68+domainLen]),
			Online:    int32(binary.LittleEndian.Uint32(data[68+domainLen : 72+domainLen])),
			Active:    data[72+domainLen] == 1,
		}
		entries = append(entries, entry)
	}

	return entries, nil
}

// UpdateNodeOnline updates the online status of a node in the registry
func (c *RegistryClient) UpdateNodeOnline(ctx context.Context, registryName string, authority solana.PublicKey, accountToUpdate solana.PublicKey, value int32) (solana.Signature, error) {
	if value < 0 {
		return solana.Signature{}, fmt.Errorf("online value must be non-negative")
	}

	// Find the registry PDA using the provided authority
	registryPDA, _, err := findRegistryPDA(c.programID, authority, registryName)
	if err != nil {
		return solana.Signature{}, fmt.Errorf("failed to find registry PDA: %v", err)
	}

	// Build the instruction
	instruction, err := buildUpdateNodeOnlineInstruction(
		c.programID,
		accountToUpdate,
		registryPDA,
		accountToUpdate,
		value,
	)
	if err != nil {
		return solana.Signature{}, fmt.Errorf("failed to build instruction: %v", err)
	}

	// Create the transaction
	recent, err := c.client.GetLatestBlockhash(ctx, rpc.CommitmentFinalized)
	if err != nil {
		return solana.Signature{}, fmt.Errorf("failed to get recent blockhash: %v", err)
	}

	tx, err := solana.NewTransaction(
		[]solana.Instruction{instruction},
		recent.Value.Blockhash,
		solana.TransactionPayer(c.signer.PublicKey()),
	)
	if err != nil {
		return solana.Signature{}, fmt.Errorf("failed to create transaction: %v", err)
	}

	// Sign and send the transaction
	_, err = tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key.Equals(c.signer.PublicKey()) {
			return &c.signer
		}
		return nil
	})
	if err != nil {
		return solana.Signature{}, fmt.Errorf("failed to sign transaction: %v", err)
	}

	wsClient, err := ws.Connect(ctx, c.wsEndpoint)
	if err != nil {
		return solana.Signature{}, fmt.Errorf("failed to connect to websocket: %v", err)
	}

	sig, err := confirm.SendAndConfirmTransaction(
		ctx,
		c.client,
		wsClient,
		tx,
	)
	if err != nil {
		return solana.Signature{}, fmt.Errorf("failed to send and confirm transaction: %v", err)
	}

	return sig, nil
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

// getClientEntry retrieves a client entry account data
func getClientEntry(
	ctx context.Context,
	client *rpc.Client,
	programID solana.PublicKey,
	registry solana.PublicKey,
	accountToCheck solana.PublicKey,
) (*ClientEntry, error) {
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
	if len(data) != ClientEntrySize {
		return nil, fmt.Errorf("invalid account data size: expected %d, got %d", ClientEntrySize, len(data))
	}

	// Skip the 8-byte discriminator
	data = data[8:]

	entry := &ClientEntry{
		Parent:    solana.PublicKeyFromBytes(data[:32]),
		Registred: solana.PublicKeyFromBytes(data[32:64]),
		Until:     int64(binary.LittleEndian.Uint64(data[64:72])),
		Limit:     binary.LittleEndian.Uint32(data[72:76]),
	}

	return entry, nil
}

// getNodeEntry retrieves a node entry account data
func getNodeEntry(
	ctx context.Context,
	client *rpc.Client,
	programID solana.PublicKey,
	registry solana.PublicKey,
	accountToCheck solana.PublicKey,
) (*NodeEntry, error) {
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
	if len(data) != NodeEntrySize {
		return nil, fmt.Errorf("invalid account data size: expected %d, got %d", NodeEntrySize, len(data))
	}

	// Skip the 8-byte discriminator
	data = data[8:]

	// Read domain string length (4 bytes)
	domainLen := binary.LittleEndian.Uint32(data[64:68])

	entry := &NodeEntry{
		Parent:    solana.PublicKeyFromBytes(data[:32]),
		Registred: solana.PublicKeyFromBytes(data[32:64]),
		Domain:    string(data[68 : 68+domainLen]),
		Online:    int32(binary.LittleEndian.Uint32(data[68+domainLen : 72+domainLen])),
		Active:    data[72+domainLen] == 1,
	}

	return entry, nil
}

// buildUpdateNodeOnlineInstruction builds the instruction to update node online status
func buildUpdateNodeOnlineInstruction(
	programID solana.PublicKey,
	authority solana.PublicKey,
	registry solana.PublicKey,
	accountToUpdate solana.PublicKey,
	value int32,
) (solana.Instruction, error) {
	entryPDA, _, err := findRegistryEntryPDA(programID, accountToUpdate, registry)
	if err != nil {
		return nil, fmt.Errorf("failed to find entry PDA: %v", err)
	}

	// Encode the instruction data
	data := new(bytes.Buffer)
	// Write instruction discriminator
	data.Write(UpdateNodeOnlineDiscriminator)
	// Encode account to update
	data.Write(accountToUpdate.Bytes())
	// Encode online value
	binary.Write(data, binary.LittleEndian, value)

	accounts := solana.AccountMetaSlice{
		solana.Meta(entryPDA).WRITE(),
		solana.Meta(registry),
		solana.Meta(authority).SIGNER().WRITE(),
	}

	return solana.NewInstruction(
		programID,
		accounts,
		data.Bytes(),
	), nil
}
