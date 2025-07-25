package service

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/gagliardetto/solana-go"

	p2p_common "github.com/dTelecom/p2p-database/common"

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
	PeerListenPort    int
	lock              sync.RWMutex
	nodeValues        map[solana.PublicKey]bootNodeMessage
}

func NewBootNodeProvider(conf *config.Config) *BootNodeProvider {
	provider := &BootNodeProvider{
		WalletPrivateKey:  conf.Solana.WalletPrivateKey,
		ContractAddress:   conf.Solana.ContractAddress,
		NetworkHostHTTP:   conf.Solana.NetworkHostHTTP,
		NetworkHostWS:     conf.Solana.NetworkHostWS,
		RegistryAuthority: conf.Solana.RegistryAuthority,
		PeerListenPort:    conf.P2P.PeerListenPort,
		lock:              sync.RWMutex{},
		nodeValues:        make(map[solana.PublicKey]bootNodeMessage),
	}

	provider.startRefresh()
	go provider.processRefresh()

	return provider
}

func (p *BootNodeProvider) GetAuthorizedWallets(ctx context.Context) ([]solana.PublicKey, error) {
	p.lock.Lock()
	defer p.lock.Unlock()

	var wallets []solana.PublicKey
	for k, v := range p.nodeValues {
		if v.isExpired() == false {
			wallets = append(wallets, k)
		}
	}

	return wallets, nil
}

func (p *BootNodeProvider) GetBootstrapNodes(ctx context.Context) ([]p2p_common.BootstrapNode, error) {
	p.lock.Lock()
	defer p.lock.Unlock()

	var nodes []p2p_common.BootstrapNode
	for k, v := range p.nodeValues {
		if v.isExpired() == false {
			node := p2p_common.BootstrapNode{
				PublicKey: k,
				IP:        v.BootNode.IP,
				QUICPort:  p.PeerListenPort,
				TCPPort:   p.PeerListenPort,
			}
			nodes = append(nodes, node)
		}
	}

	return nodes, nil
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
		p.save(entryNode.Registred, node)
	}
	return nil
}

func (p *BootNodeProvider) save(id solana.PublicKey, node BootNode) {
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
