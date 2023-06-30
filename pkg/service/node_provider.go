package service

import (
	"time"

	p2p_database "github.com/dTelecom/p2p-realtime-database"
	"github.com/oschwald/geoip2-golang"
)

type NodeProvider struct {
	geo *geoip2.Reader
}

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

func (p *NodeProvider) Save(db *p2p_database.DB, node Node) error {
}

func (p *NodeProvider) Get(db *p2p_database.DB, id string) (Node, error) {
}
