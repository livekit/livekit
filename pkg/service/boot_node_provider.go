package service

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/gagliardetto/solana-go"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/protocol/logger"
)

type BootNode struct {
	Domain string
	IP     string
}

type bootNodeMessage struct {
	BootNode BootNode
	TTL      int64
}

func (v *bootNodeMessage) isExpired() bool {
	return time.Now().Unix() >= v.TTL
}

const (
	bootNodeTtl             = 300 * time.Second
	bootNodeRefreshInterval = 100 * time.Second
)

type BootNodeProvider struct {
	WalletPrivateKey  string
	ContractAddress   string
	NetworkHostHTTP   string
	NetworkHostWS     string
	RegistryAuthority string
	lock              sync.RWMutex
	nodeValues        map[string]bootNodeMessage
}

func NewBootNodeProvider(conf config.SolanaConfig) *BootNodeProvider {
	provider := &BootNodeProvider{
		WalletPrivateKey:  conf.WalletPrivateKey,
		ContractAddress:   conf.ContractAddress,
		NetworkHostHTTP:   conf.NetworkHostHTTP,
		NetworkHostWS:     conf.NetworkHostWS,
		RegistryAuthority: conf.RegistryAuthority,
		lock:              sync.RWMutex{},
		nodeValues:        make(map[string]bootNodeMessage),
	}

	provider.startRefresh()
	go provider.processRefresh()

	return provider
}

func (p *BootNodeProvider) GetNodes() map[string]string {
	p.lock.Lock()
	defer p.lock.Unlock()

	nodes := make(map[string]string)
	for k, v := range p.nodeValues {
		if v.isExpired() == false {
			nodes[k] = v.BootNode.IP
		}
	}

	return nodes
}

func (p *BootNodeProvider) refresh(ctx context.Context) error {
	authority, err := solana.PublicKeyFromBase58(p.RegistryAuthority)
	if err != nil {
		return err
	}

	client, err := NewRegistryClient(p.NetworkHostHTTP, p.NetworkHostWS, p.ContractAddress, p.WalletPrivateKey)
	if err != nil {
		return err
	}

	defer client.Close()

	entryNodes, err := client.ListNodesInRegistry(ctx, authority, "dtel-nodes")
	if err != nil {
		return err
	}
	for _, entryNode := range entryNodes {
		if entryNode.Active == false {
			continue
		}

		var ipv4 net.IP
		ips, _ := net.LookupIP(entryNode.Domain)
		for _, ip := range ips {
			ipv4 = ip.To4()
			if ipv4 != nil {
				break
			}
		}
		if ipv4 == nil {
			logger.Errorw("ipv4 nil", fmt.Errorf("domain error: %v", entryNode.Domain))
			continue
		}

		node := BootNode{
			Domain: entryNode.Domain,
			IP:     ipv4.String(),
		}
		p.save(entryNode.Registred.String(), node)
	}
	return nil
}

func (p *BootNodeProvider) save(id string, node BootNode) {
	p.lock.Lock()
	defer p.lock.Unlock()
	msg := bootNodeMessage{
		BootNode: node,
		TTL:      time.Now().Add(bootNodeTtl).Unix(),
	}
	p.nodeValues[id] = msg
}

func (p *BootNodeProvider) startRefresh() {
	go func() {
		ticker := time.NewTicker(bootNodeRefreshInterval)
		for {
			<-ticker.C
			p.processRefresh()
		}
	}()
}

func (p *BootNodeProvider) processRefresh() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()
	err := p.refresh(ctx)
	if err != nil {
		logger.Errorw("[refresh] error %s\r\n", err)
	}
}
