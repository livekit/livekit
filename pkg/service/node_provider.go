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
)

const prefixKeyNode = "node_"

type Node struct {
	Id           string    `json:"id"`
	Participants int       `json:"participants"`
	Domain       string    `json:"domain"`
	IP           string    `json:"ip"`
	Country      string    `json:"country"`
	Latitude     float64   `json:"latitude"`
	Longitude    float64   `json:"longitude"`
	CreatedAt    time.Time `json:"created_at"`
}

const (
	weightEqualsCountries   = 1.0
	weightParticipantsCount = -0.1
	weightDistance          = -0.001

	intervalPingNode = 5 * time.Second
	deadlinePingNode = 30 * time.Second
)

type NodeProvider struct {
	db     *p2p_database.DB
	geo    *geoip2.Reader
	logger *log.ZapEventLogger
}

func NewNodeProvider(db *p2p_database.DB, geo *geoip2.Reader, logger *log.ZapEventLogger) *NodeProvider {
	provider := &NodeProvider{
		db:     db,
		geo:    geo,
		logger: logger,
	}

	backgroundCtx := context.Background()

	provider.refreshTTL(backgroundCtx)

	return provider
}

func (p *NodeProvider) FetchRelevant(ctx context.Context, clientIP string) (Node, error) {
	keys, err := p.db.List(ctx)
	if err != nil {
		return Node{}, errors.Wrap(err, "list keys")
	}

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

	for _, key := range keys {
		if !strings.HasPrefix(key, "/"+prefixKeyNode) {
			continue
		}
		nodeId := strings.TrimLeft(key, "/"+prefixKeyNode)

		node, err := p.Get(ctx, nodeId)
		if err != nil {
			return Node{}, errors.Wrap(err, "get node by id")
		}

		var weight float64
		if clientCountry == node.Country {
			weight += weightEqualsCountries
		}
		dist := distance(node.Latitude, node.Longitude, clientLat, clientLon)
		weight = dist*weightDistance + float64(node.Participants)*weightParticipantsCount

		p.logger.Debugf(
			"calculated weight for node %s is %f (%s - %s, %f, %d)",
			nodeId,
			dist,
			clientCountry,
			node.Country,
			dist,
			node.Participants,
		)

		nodes = append(nodes, nodeRow{node: node, weight: weight})
	}

	sort.SliceStable(nodes, func(i, j int) bool { return nodes[i].weight > nodes[j].weight })

	if len(nodes) == 0 {
		return Node{}, errors.New("not found node")
	}

	return nodes[0].node, nil
}

func (p *NodeProvider) IncrementParticipants(ctx context.Context, nodeId string) error {
	node, err := p.Get(ctx, nodeId)
	if err != nil {
		return errors.Wrap(err, "get current value")
	}
	node.Participants++
	return p.save(ctx, node)
}

func (p *NodeProvider) DecrementParticipants(ctx context.Context, nodeId string) error {
	node, err := p.Get(ctx, nodeId)
	if err != nil {
		return errors.Wrap(err, "get current value")
	}
	node.Participants--
	return p.save(ctx, node)
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

	return p.save(ctx, node)
}

func (p *NodeProvider) RemoveCurrentNode(ctx context.Context) error {
	k := prefixKeyNode + p.db.GetHost().ID().String()
	err := p.db.Remove(ctx, k)
	if err != nil {
		return errors.Wrap(err, "remove p2p key")
	}
	return nil
}

func (p *NodeProvider) Get(ctx context.Context, id string) (Node, error) {
	var res Node

	row, err := p.db.Get(ctx, prefixKeyNode+id)
	if err != nil {
		return Node{}, errors.Wrap(err, "p2p db get")
	}

	err = json.Unmarshal([]byte(row), &res)
	if err != nil {
		return Node{}, errors.Wrap(err, "unmarshal row")
	}

	return res, nil
}

func (p *NodeProvider) save(ctx context.Context, node Node) error {
	k := prefixKeyNode + node.Id

	marshaled, err := json.Marshal(node)
	if err != nil {
		return errors.Wrap(err, "marshal node")
	}

	err = p.db.Set(ctx, k, string(marshaled))
	if err != nil {
		return errors.Wrap(err, "p2p db set")
	}

	err = p.db.TTL(ctx, k, deadlinePingNode)
	if err != nil {
		return errors.Wrap(err, "p2p db ttl")
	}

	return nil
}

func (p *NodeProvider) refreshTTL(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(intervalPingNode)
		for {
			k := prefixKeyNode + p.db.GetHost().ID().String()
			err := p.db.TTL(ctx, k, deadlinePingNode)
			if err != nil {
				p.logger.Errorw("refresh ttl", err)
			}
			<-ticker.C
		}
	}()
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
