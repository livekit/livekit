package main

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/utils"
	"github.com/olekukonko/tablewriter"
	"github.com/urfave/cli/v2"
	"gopkg.in/yaml.v3"

	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/service"
)

func generateKeys(c *cli.Context) error {
	apiKey := utils.NewGuid(utils.APIKeyPrefix)
	secret := utils.RandomSecret()
	fmt.Println("API Key: ", apiKey)
	fmt.Println("API Secret: ", secret)
	return nil
}

func printPorts(c *cli.Context) error {
	conf, err := getConfig(c)
	if err != nil {
		return err
	}

	udpPorts := make([]string, 0)
	tcpPorts := make([]string, 0)

	tcpPorts = append(tcpPorts, fmt.Sprintf("%d - HTTP service", conf.Port))
	if conf.RTC.TCPPort != 0 {
		tcpPorts = append(tcpPorts, fmt.Sprintf("%d - ICE/TCP", conf.RTC.TCPPort))
	}
	if conf.RTC.UDPPort != 0 {
		udpPorts = append(udpPorts, fmt.Sprintf("%d - ICE/UDP", conf.RTC.UDPPort))
	} else {
		udpPorts = append(udpPorts, fmt.Sprintf("%d-%d - ICE/UDP range", conf.RTC.ICEPortRangeStart, conf.RTC.ICEPortRangeEnd))
	}

	if conf.TURN.Enabled {
		if conf.TURN.TLSPort > 0 {
			tcpPorts = append(tcpPorts, fmt.Sprintf("%d - TURN/TLS", conf.TURN.TLSPort))
		}
		if conf.TURN.UDPPort > 0 {
			udpPorts = append(udpPorts, fmt.Sprintf("%d - TURN/UDP", conf.TURN.UDPPort))
		}
	}

	fmt.Println("TCP Ports")
	for _, p := range tcpPorts {
		fmt.Println(p)
	}

	fmt.Println("UDP Ports")
	for _, p := range udpPorts {
		fmt.Println(p)
	}
	return nil
}

func createToken(c *cli.Context) error {
	room := c.String("room")
	identity := c.String("identity")

	conf, err := getConfig(c)
	if err != nil {
		return err
	}

	// use the first API key from config
	if len(conf.Keys) == 0 {
		// try to load from file
		if _, err := os.Stat(conf.KeyFile); err != nil {
			return err
		}
		f, err := os.Open(conf.KeyFile)
		if err != nil {
			return err
		}
		defer func() {
			_ = f.Close()
		}()
		decoder := yaml.NewDecoder(f)
		if err = decoder.Decode(conf.Keys); err != nil {
			return err
		}

		if len(conf.Keys) == 0 {
			return fmt.Errorf("keys are not configured")
		}
	}

	var apiKey string
	var apiSecret string
	for k, v := range conf.Keys {
		apiKey = k
		apiSecret = v
		break
	}

	grant := &auth.VideoGrant{
		RoomJoin: true,
		Room:     room,
	}
	if c.Bool("recorder") {
		grant.Hidden = true
		grant.SetCanPublish(false)
		grant.SetCanPublishData(false)
	}

	at := auth.NewAccessToken(apiKey, apiSecret).
		AddGrant(grant).
		SetIdentity(identity).
		SetValidFor(30 * 24 * time.Hour)

	token, err := at.ToJWT()
	if err != nil {
		return err
	}

	fmt.Println("Token:", token)

	return nil
}

func listNodes(c *cli.Context) error {
	conf, err := getConfig(c)
	if err != nil {
		return err
	}

	currentNode, err := routing.NewLocalNode(conf)
	if err != nil {
		return err
	}

	router, err := service.InitializeRouter(conf, currentNode)
	if err != nil {
		return err
	}

	nodes, err := router.ListNodes()
	if err != nil {
		return err
	}

	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{
		"ID", "IP Address", "Region",
		"CPUs", "Load",
		"Clients", "Rooms", "Tracks In/Out",
		"Bytes In/Out", "Packets In/Out", "Nack", "Bps In/Out", "Pps In/Out", "Nack/Sec",
		"Started At", "Updated At",
	})
	for _, node := range nodes {
		// System stats
		cpus := strconv.Itoa(int(node.Stats.NumCpus))
		loadAvg := fmt.Sprintf("%.2f, %.2f, %.2f", node.Stats.LoadAvgLast1Min, node.Stats.LoadAvgLast5Min, node.Stats.LoadAvgLast15Min)

		// Room stats
		clients := strconv.Itoa(int(node.Stats.NumClients))
		rooms := strconv.Itoa(int(node.Stats.NumRooms))
		tracks := fmt.Sprintf("%d / %d", node.Stats.NumTracksIn, node.Stats.NumTracksOut)

		// Packet stats
		bytes := fmt.Sprintf("%d / %d", node.Stats.BytesIn, node.Stats.BytesOut)
		packets := fmt.Sprintf("%d / %d", node.Stats.PacketsIn, node.Stats.PacketsOut)
		nack := strconv.Itoa(int(node.Stats.NackTotal))
		bps := fmt.Sprintf("%.2f / %.2f", node.Stats.BytesInPerSec, node.Stats.BytesOutPerSec)
		packetsPerSec := fmt.Sprintf("%.2f / %.2f", node.Stats.PacketsInPerSec, node.Stats.PacketsOutPerSec)
		nackPerSec := fmt.Sprintf("%f", node.Stats.NackPerSec)

		startedAt := time.Unix(node.Stats.StartedAt, 0).String()
		updatedAt := time.Unix(node.Stats.UpdatedAt, 0).String()

		table.Append([]string{
			node.Id, node.Ip, node.Region,
			cpus, loadAvg,
			clients, rooms, tracks,
			bytes, packets, nack, bps, packetsPerSec, nackPerSec,
			startedAt, updatedAt,
		})
	}
	table.Render()

	return nil
}
