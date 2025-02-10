package service

import (
	"context"
	"encoding/json"
	"math"
	"net"
	"sort"
	"sync"
	"time"

	p2p_database "github.com/dTelecom/p2p-realtime-database"
	"github.com/ipfs/go-log/v2"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/protocol/livekit"
	"github.com/oschwald/geoip2-golang"
	"github.com/pkg/errors"
)

const prefixKeyNode = "node_"

type Node struct {
	Id           string    `json:"id"`
	Participants int32     `json:"participants"`
	Domain       string    `json:"domain"`
	IP           string    `json:"ip"`
	Country      string    `json:"country"`
	Latitude     float64   `json:"latitude"`
	Longitude    float64   `json:"longitude"`
	CreatedAt    time.Time `json:"created_at"`
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
	mainDatabase *p2p_database.DB
	geo          *geoip2.Reader
	logger       *log.ZapEventLogger
	current      Node
	localNode    routing.LocalNode
	prevStats    *livekit.NodeStats
	lock         sync.RWMutex
	nodeValues   map[string]nodeMessage
}

func NewNodeProvider(mainDatabase *p2p_database.DB, geo *geoip2.Reader, logger *log.ZapEventLogger, localNode routing.LocalNode) *NodeProvider {
	provider := &NodeProvider{
		mainDatabase: mainDatabase,
		geo:          geo,
		logger:       logger,
		localNode:    localNode,
		lock:         sync.RWMutex{},
		nodeValues:   make(map[string]nodeMessage),
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()

	err := mainDatabase.Subscribe(ctx, topicNodeValuesStreaming, func(event p2p_database.Event) {
		jsonMsg, ok := event.Message.(string)
		if !ok {
			logger.Errorw("convert interface to string from message topic node values")
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

	logger.Errorw("topic node values subscribe error", err)

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

	for _, xNode := range xNodes {
		var weight float64
		if clientCountry == xNode.Country {
			weight += weightEqualsCountries
		}
		dist := distance(xNode.Latitude, xNode.Longitude, clientLat, clientLon)
		weight = dist*weightDistance + float64(xNode.Participants)*weightParticipantsCount

		p.logger.Infow(
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
		return Node{}, errors.New("not found node")
	}

	return nodes[0].node, nil
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

	node.Country = country.Country.IsoCode
	node.Latitude = city.Location.Latitude
	node.Longitude = city.Location.Longitude
	node.CreatedAt = time.Now().UTC()
	node.Participants = p.localNode.Stats.NumClients
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

	_, err = p.mainDatabase.Publish(ctx, topicNodeValuesStreaming, string(marshaled))
	if err != nil {
		return errors.Wrap(err, "publish traffic message")
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
			ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
			defer cancel()
			err := p.refresh(ctx)
			if err != nil {
				p.logger.Errorw("[refresh] error %s\r\n", err)
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
		p.logger.Errorw("could not update node stats", err)
	}
	p.localNode.Stats = updated
	if computedAvg {
		p.prevStats = updated
	}
}
