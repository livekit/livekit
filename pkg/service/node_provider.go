package service

import (
	"context"
	"encoding/json"
	"math"
	"net"
	"sort"
	"strings"
	"time"

	p2p_database "github.com/dTelecom/p2p-realtime-database"
	"github.com/ipfs/go-log/v2"
	"github.com/oschwald/geoip2-golang"
	"github.com/pkg/errors"
	"github.com/livekit/livekit-server/pkg/routing"
)

const prefixKeyNode = "node_"

type Node struct {
	Id           string    `json:"id"`
	Participants int32       `json:"participants"`
	Domain       string    `json:"domain"`
	IP           string    `json:"ip"`
	Country      string    `json:"country"`
	Latitude     float64   `json:"latitude"`
	Longitude    float64   `json:"longitude"`
	CreatedAt    time.Time `json:"created_at"`
}

type rowNodeDatabaseRecord struct {
	Node Node    `json:"node"`
	TTL    time.Time `json:"ttl"`
}

const (
	weightEqualsCountries   = 1.0
	weightParticipantsCount = -0.1
	weightDistance          = -0.001
	defaultNodeTtl                           = 2 * time.Minute
	defaultNodeIntervalCheckingExpiredRecord = 1 * time.Minute
	nodeRefreshInterval = 10 * time.Second
)

type NodeProvider struct {
	mainDatabase     *p2p_database.DB
	geo    *geoip2.Reader
	logger *log.ZapEventLogger
	current Node
	localNode routing.LocalNode
}

func NewNodeProvider(mainDatabase *p2p_database.DB, geo *geoip2.Reader, logger *log.ZapEventLogger, localNode routing.LocalNode) *NodeProvider {
	provider := &NodeProvider{
		mainDatabase:     mainDatabase,
		geo:    geo,
		logger: logger,
		localNode: localNode,
	}

	provider.startRemovingExpiredRecord()
	provider.startRefresh()

	return provider
}

func (p *NodeProvider) List(ctx context.Context) ([]Node, error) {
	keys, err := p.mainDatabase.List(ctx)
	if err != nil {
		return []Node{}, errors.Wrap(err, "list keys")
	}

	var nodes []Node
	for _, k := range keys {
		if !strings.HasPrefix(k, "/"+prefixKeyNode) {
			continue
		}
		nodeId := strings.TrimLeft(k, "/"+prefixKeyNode)
		node, err := p.Get(ctx, nodeId)
		if err != nil {
			p.logger.Errorw("key not found "+k, err)
			continue
		}
		nodes = append(nodes, node)
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
			"calculated weight for node %s is %f (%s - %s, %f, %d)",
			xNode.Id,
			dist,
			clientCountry,
			xNode.Country,
			dist,
			xNode.Participants,
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
	node.CreatedAt = time.Now()
	node.Participants = p.localNode.Stats.NumClients
	p.current = node

	return p.save(ctx, node)
}

func (p *NodeProvider) refresh(ctx context.Context) error {
	node := p.current
	node.Participants = p.localNode.Stats.NumClients

	return p.save(ctx, node)
}


func (p *NodeProvider) RemoveCurrentNode(ctx context.Context) error {
	k := prefixKeyNode + p.mainDatabase.GetHost().ID().String()
	err := p.mainDatabase.Remove(ctx, k)
	if err != nil {
		return errors.Wrap(err, "remove p2p key")
	}
	return nil
}

func (p *NodeProvider) Get(ctx context.Context, id string) (Node, error) {
	return p.getFromDatabase(ctx, id)
}

func (p *NodeProvider) getFromDatabase(ctx context.Context, id string) (Node, error) {
	key := prefixKeyNode + id

	row, err := p.mainDatabase.Get(ctx, key)
	if err != nil {
		return Node{}, errors.Wrap(err, "get row")
	}

	var result rowNodeDatabaseRecord
	err = json.Unmarshal([]byte(row), &result)
	if err != nil {
		return Node{}, errors.Wrap(err, "unmarshal record")
	}

	if result.TTL.Before(time.Now().UTC()) {
		err = p.mainDatabase.Remove(ctx, key)
		if err != nil {
			return Node{}, errors.Wrap(err, "remove expired record")
		}
		return Node{}, errors.New("client expired")
	}

	return result.Node, nil
}

func (p *NodeProvider) save(ctx context.Context, node Node) error {
	record := rowNodeDatabaseRecord{
		Node: node,
		TTL:    time.Now().UTC().Add(defaultNodeTtl),
	}

	k := prefixKeyNode + node.Id

	marshaled, err := json.Marshal(record)
	if err != nil {
		return errors.Wrap(err, "marshal node")
	}

	err = p.mainDatabase.Set(ctx, k, string(marshaled))
	if err != nil {
		return errors.Wrap(err, "p2p db set")
	}

	return nil
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

func (p *NodeProvider) startRemovingExpiredRecord() {
	go func() {
		ctx := context.Background()

		ticker := time.NewTicker(defaultNodeIntervalCheckingExpiredRecord)
		for {
			keys, err := p.mainDatabase.List(ctx)
			if err != nil {
				p.logger.Errorw("[startRemovingExpiredRecord] error list main databases keys %s\r\n", err)
				return
			}

			for _, key := range keys {
				key = strings.TrimPrefix("/", key)
				if !strings.HasPrefix(key, prefixKeyNode) {
					continue
				}

				row, err := p.mainDatabase.Get(ctx, key)
				if err != nil {
					p.logger.Errorw("[startRemovingExpiredRecord] get database record with key %s error %s\r\n", key, err)
					continue
				}

				var result rowNodeDatabaseRecord
				err = json.Unmarshal([]byte(row), &result)
				if err != nil {
					p.logger.Errorw("[startRemovingExpiredRecord] unmarshal record with key %s error %s\r\n", key, err)
					continue
				}

				if result.TTL.Before(time.Now().UTC()) {
					err = p.mainDatabase.Remove(ctx, key)
					if err != nil {
						p.logger.Errorw("[startRemovingExpiredRecord] remove expired record with key %s error %s\r\n", key, err)
						continue
					}
				}
			}

			<-ticker.C
		}
	}()
}

func (p *NodeProvider) startRefresh() {
	go func() {
		ctx := context.Background()

		ticker := time.NewTicker(nodeRefreshInterval)
		for {
			<-ticker.C
			err := p.refresh(ctx)
			if err != nil {
				p.logger.Errorw("[refresh] error %s\r\n", err)
				continue
			}
		}
	}()
}
