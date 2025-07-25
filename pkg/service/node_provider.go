package service

import (
	"context"
	"encoding/json"
	"math"
	"net"
	"sort"
	"sync"
	"time"

	p2p_common "github.com/dTelecom/p2p-database/common"
	"github.com/dTelecom/p2p-database/pubsub"

	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/oschwald/geoip2-golang"
	"github.com/pkg/errors"
)

type Node struct {
	Id           string  `json:"id"`
	Participants int32   `json:"participants"`
	Domain       string  `json:"domain"`
	IP           string  `json:"ip"`
	Country      string  `json:"country"`
	City         string  `json:"city"`
	Latitude     float64 `json:"latitude"`
	Longitude    float64 `json:"longitude"`
}

type nodeMessage struct {
	Node Node  `json:"node"`
	TTL  int64 `json:"ttl"`
}

func (v *nodeMessage) isExpired() bool {
	return time.Now().Unix() >= v.TTL
}

const (
	weightEqualsCountries    = 1.0
	weightParticipantsCount  = -0.1
	weightDistance           = -0.001
	defaultNodeTtl           = 30 * time.Second
	nodeRefreshInterval      = 10 * time.Second
	topicNodeValuesStreaming = "nodes_values"
)

type NodeProvider struct {
	db         *pubsub.DB
	geo        *geoip2.Reader
	current    Node
	localNode  routing.LocalNode
	prevStats  *livekit.NodeStats
	lock       sync.RWMutex
	nodeValues map[string]nodeMessage
}

func NewNodeProvider(geo *geoip2.Reader, localNode routing.LocalNode, db *pubsub.DB) *NodeProvider {
	provider := &NodeProvider{
		db:         db,
		geo:        geo,
		localNode:  localNode,
		lock:       sync.RWMutex{},
		nodeValues: make(map[string]nodeMessage),
	}

	err := db.Subscribe(context.Background(), topicNodeValuesStreaming, func(event p2p_common.Event) {
		jsonMsg, ok := event.Message.(string)
		if !ok {
			logger.Errorw("convert interface to string from message topic node values", errors.New("NewNodeProvider"))
			return
		}

		nodeMsg := nodeMessage{}
		err := json.Unmarshal([]byte(jsonMsg), &nodeMsg)
		if err != nil {
			logger.Errorw("topic node values unmarshal error", err)
			return
		}

		provider.handleNodeMessage(nodeMsg, event.FromPeerId)
	})

	if err != nil {
		logger.Errorw("topic node values subscribe error", err)
	}

	provider.startRefresh()

	return provider
}

func (p *NodeProvider) List(ctx context.Context) ([]Node, error) {
	p.lock.Lock()
	defer p.lock.Unlock()

	var nodes []Node
	for _, v := range p.nodeValues {
		if v.isExpired() == false {
			nodes = append(nodes, v.Node)
		}
	}

	return nodes, nil
}

func (p *NodeProvider) FetchRelevant(ctx context.Context, clientIP string) (Node, error) {
	nodes, err := p.FetchRelevantNodes(ctx, clientIP)
	if err != nil {
		return Node{}, err
	}

	return nodes[0], nil
}

func (p *NodeProvider) FetchRelevantNodes(ctx context.Context, clientIP string) ([]Node, error) {
	ip := net.ParseIP(clientIP)
	city, err := p.geo.City(ip)

	var result []Node

	if err != nil {
		return result, errors.Wrap(err, "fetch city")
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
		return result, errors.Wrap(err, "list nodes")
	}

	if len(xNodes) < 2 {
		return result, errors.New("not enough nodes")
	}

	for _, xNode := range xNodes {
		var weight float64
		if clientCountry == xNode.Country {
			weight += weightEqualsCountries
		}
		dist := distance(xNode.Latitude, xNode.Longitude, clientLat, clientLon)
		weight = dist*weightDistance + float64(xNode.Participants)*weightParticipantsCount

		logger.Infow(
			"calculated weight for",
			"node id", xNode.Id,
			"distance", dist,
			"client country", clientCountry,
			"node country", xNode.Country,
			"node participants", xNode.Participants,
		)

		nodes = append(nodes, nodeRow{node: xNode, weight: weight})
	}

	sort.SliceStable(nodes, func(i, j int) bool { return nodes[i].weight > nodes[j].weight })

	if len(nodes) == 0 {
		return result, errors.New("not found node")
	}

	for i, node := range nodes {
		if i > 3 {
			break
		}
		result = append(result, node.node)
	}

	return result, nil
}

func (p *NodeProvider) Save(ctx context.Context, node Node) error {
	ip := net.ParseIP(node.IP)

	country, err := p.geo.Country(ip)
	if err != nil {
		return errors.Wrap(err, "fetch country")
	}

	city, err := p.geo.City(ip)
	if err != nil {
		return errors.Wrap(err, "fetch city")
	}

	node.Country = country.Country.Names["en"]
	node.City = city.City.Names["en"]
	node.Latitude = city.Location.Latitude
	node.Longitude = city.Location.Longitude
	node.Participants = p.localNode.Stats.NumClients
	node.Id = p.db.GetHost().ID().String()
	p.current = node

	return p.save(ctx, node)
}

func (p *NodeProvider) refresh(ctx context.Context) error {
	node := p.current
	node.Participants = p.localNode.Stats.NumClients

	return p.save(ctx, node)
}

func (p *NodeProvider) save(ctx context.Context, node Node) error {
	record := nodeMessage{
		Node: node,
		TTL:  time.Now().Add(defaultNodeTtl).Unix(),
	}

	p.handleNodeMessage(record, node.Id)

	marshaled, err := json.Marshal(record)
	if err != nil {
		return errors.Wrap(err, "marshal node")
	}

	_, err = p.db.Publish(ctx, topicNodeValuesStreaming, string(marshaled))
	if err != nil {
		return errors.Wrap(err, "publish node message")
	}

	return nil
}

func (p *NodeProvider) handleNodeMessage(nodeMsg nodeMessage, fromPeerId string) {
	p.lock.Lock()
	defer p.lock.Unlock()
	p.nodeValues[fromPeerId] = nodeMsg
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
			p.UpdateNodeStats()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second*15)
			defer cancel()
			err := p.refresh(ctx)
			if err != nil {
				logger.Errorw("[refresh] error %s\r\n", err)
				continue
			}
		}
	}()
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
