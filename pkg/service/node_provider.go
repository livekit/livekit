package service

import (
	"context"
	"fmt"
	"math"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/gagliardetto/solana-go"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/oschwald/geoip2-golang"
	"github.com/pkg/errors"
)

type Node struct {
	Participants int32
	Domain       string
	IP           string
	Country      string
	City         string
	Latitude     float64
	Longitude    float64
}

type nodeMessage struct {
	Node Node
	TTL  int64
}

func (v *nodeMessage) isExpired() bool {
	return time.Now().Unix() >= v.TTL
}

const (
	weightEqualsCountries   = 1.0
	weightParticipantsCount = -0.1
	weightDistance          = -0.001
	defaultNodeTtl          = 30 * time.Second
	nodeRefreshInterval     = 10 * time.Second
)

type NodeProvider struct {
	WalletPrivateKey  string
	ContractAddress   string
	NetworkHostHTTP   string
	NetworkHostWS     string
	RegistryAuthority string
	geo               *geoip2.Reader
	current           Node
	localNode         routing.LocalNode
	prevStats         *livekit.NodeStats
	lock              sync.RWMutex
	nodeValues        map[string]nodeMessage
}

func NewNodeProvider(geo *geoip2.Reader, localNode routing.LocalNode, conf config.SolanaConfig) *NodeProvider {
	provider := &NodeProvider{
		WalletPrivateKey:  conf.WalletPrivateKey,
		ContractAddress:   conf.ContractAddress,
		NetworkHostHTTP:   conf.NetworkHostHTTP,
		NetworkHostWS:     conf.NetworkHostWS,
		RegistryAuthority: conf.RegistryAuthority,
		geo:               geo,
		localNode:         localNode,
		lock:              sync.RWMutex{},
		nodeValues:        make(map[string]nodeMessage),
	}

	provider.startRefresh()
	go provider.processRefresh()

	return provider
}

func (p *NodeProvider) List(ctx context.Context) (map[string]Node, error) {
	p.lock.Lock()
	defer p.lock.Unlock()

	nodes := make(map[string]Node)
	for k, v := range p.nodeValues {
		if v.isExpired() == false {
			nodes[k] = v.Node
		}
	}

	return nodes, nil
}

func (p *NodeProvider) GetNodes() map[string]string {
	p.lock.Lock()
	defer p.lock.Unlock()

	nodes := make(map[string]string)
	for k, v := range p.nodeValues {
		if v.isExpired() == false {
			nodes[k] = v.Node.IP
		}
	}

	return nodes
}

func (p *NodeProvider) FetchRelevant(ctx context.Context, clientIP string) (Node, error) {
	ip := net.ParseIP(clientIP)
	city, err := p.geo.City(ip)
	if err != nil {
		return Node{}, errors.Wrap(err, "fetch city")
	}
	clientLat := city.Location.Latitude
	clientLon := city.Location.Longitude
	clientCountry := city.Country.IsoCode

	type nodeRow struct {
		node   Node
		weight float64
	}
	var nodes []nodeRow
	xNodes, err := p.List(ctx)
	if err != nil {
		return Node{}, errors.Wrap(err, "list nodes")
	}

	for k, xNode := range xNodes {
		var weight float64
		if clientCountry == xNode.Country {
			weight += weightEqualsCountries
		}
		dist := distance(xNode.Latitude, xNode.Longitude, clientLat, clientLon)
		weight = dist*weightDistance + float64(xNode.Participants)*weightParticipantsCount

		logger.Infow(
			"calculated weight for",
			"node id", k,
			"distance", dist,
			"client country", clientCountry,
			"node country", xNode.Country,
			"node participants", xNode.Participants,
		)

		nodes = append(nodes, nodeRow{node: xNode, weight: weight})
	}

	sort.SliceStable(nodes, func(i, j int) bool { return nodes[i].weight > nodes[j].weight })

	if len(nodes) == 0 {
		return Node{}, errors.New("not found node")
	}

	return nodes[0].node, nil
}

func (p *NodeProvider) refresh(ctx context.Context) error {
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

		country, err := p.geo.Country(ipv4)
		if err != nil {
			logger.Errorw("country", err)
			continue
		}

		city, err := p.geo.City(ipv4)
		if err != nil {
			logger.Errorw("city", err)
			continue
		}

		node := Node{
			Participants: entryNode.Online,
			Domain:       entryNode.Domain,
			IP:           ipv4.String(),
			Country:      country.Country.Names["en"],
			City:         city.City.Names["en"],
			Latitude:     city.Location.Latitude,
			Longitude:    city.Location.Longitude,
		}
		p.save(entryNode.Registred.String(), node)
	}
	return nil
}

func (p *NodeProvider) save(id string, node Node) {
	p.lock.Lock()
	defer p.lock.Unlock()
	msg := nodeMessage{
		Node: node,
		TTL:  time.Now().Add(defaultClientTtl).Unix(),
	}
	p.nodeValues[id] = msg
}

func distance(lat1 float64, lng1 float64, lat2 float64, lng2 float64) float64 {
	radlat1 := math.Pi * lat1 / 180
	radlat2 := math.Pi * lat2 / 180

	theta := lng1 - lng2
	radtheta := math.Pi * theta / 180

	dist := math.Sin(radlat1)*math.Sin(radlat2) + math.Cos(radlat1)*math.Cos(radlat2)*math.Cos(radtheta)
	if dist > 1 {
		dist = 1
	}

	dist = math.Acos(dist)
	dist = dist * 180 / math.Pi
	dist = dist * 60 * 1.1515
	dist = dist * 1.609344 //km

	return dist
}

func (p *NodeProvider) startRefresh() {
	go func() {
		ticker := time.NewTicker(nodeRefreshInterval)
		for {
			<-ticker.C
			p.processRefresh()
		}
	}()
}

func (p *NodeProvider) processRefresh() {
	p.UpdateNodeStats()
	ctx1, cancel1 := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel1()
	err := p.selfRefresh(ctx1)
	if err != nil {
		logger.Errorw("[selfRefresh] error %s\r\n", err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel2()
	err = p.refresh(ctx2)
	if err != nil {
		logger.Errorw("[refresh] error %s\r\n", err)
	}
}

func (p *NodeProvider) UpdateNodeStats() {

	if p.prevStats == nil {
		p.prevStats = p.localNode.Stats
	}

	updated, computedAvg, err := prometheus.GetUpdatedNodeStats(p.localNode.Stats, p.prevStats)
	if err != nil {
		logger.Errorw("could not update node stats", err)
	}
	p.localNode.Stats = updated
	if computedAvg {
		p.prevStats = updated
	}
}
