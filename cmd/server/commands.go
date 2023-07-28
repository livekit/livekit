package main

import (
	"context"
	"fmt"
	"os"
	"time"

	livekit2 "github.com/livekit/livekit-server"
	"github.com/olekukonko/tablewriter"
	"github.com/oschwald/geoip2-golang"
	"github.com/pkg/errors"
	"github.com/urfave/cli/v2"
	"gopkg.in/yaml.v3"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/utils"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/service"
)

func generateKeys(_ *cli.Context) error {
	apiKey := utils.NewGuid(utils.APIKeyPrefix)
	secret := utils.RandomSecret()
	fmt.Println("API Key: ", apiKey)
	fmt.Println("API Secret: ", secret)
	return nil
}

func listConnectedPeers(c *cli.Context) error {
	conf, err := getConfig(c)
	if err != nil {
		return errors.Wrap(err, "get config")
	}

	databaseConfig := service.GetDatabaseConfiguration(conf)
	db, err := service.CreateMainDatabaseP2P(databaseConfig, conf)
	if err != nil {
		return errors.Wrap(err, "connect main db")
	}

	table := tablewriter.NewWriter(os.Stdout)
	table.SetRowLine(true)
	table.SetAutoWrapText(false)
	table.SetHeader([]string{
		"ID",
		"Remote address",
	})
	table.SetColumnAlignment([]int{
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_CENTER,
	})

	for _, node := range db.ConnectedPeers() {
		table.Append([]string{
			node.ID.String(),
			node.Addrs[0].String(),
		})
	}

	table.Render()
	return nil
}

func listP2pNodes(c *cli.Context) error {
	conf, err := getConfig(c)
	if err != nil {
		return errors.Wrap(err, "get config")
	}

	databaseConfig := service.GetDatabaseConfiguration(conf)
	db, err := service.CreateMainDatabaseP2P(databaseConfig, conf)
	if err != nil {
		return errors.Wrap(err, "connect main db")
	}
	geoIp, err := geoip2.FromBytes(livekit2.MixmindDatabase)
	if err != nil {
		return errors.Wrap(err, "create mixmind")
	}
	nodeProvider := service.CreateNodeProvider(geoIp, conf, db)
	nodes, err := nodeProvider.List(context.Background())
	if err != nil {
		return errors.Wrap(err, "list nodes")
	}

	fmt.Println("Waiting p2p database sync")
	time.Sleep(5 * time.Second)
	fmt.Println("Done waiting p2p database sync")

	table := tablewriter.NewWriter(os.Stdout)
	table.SetRowLine(true)
	table.SetAutoWrapText(false)
	table.SetHeader([]string{
		"ID",
		"Participants",
		"Domain",
		"IP",
		"Country",
		"Latitude",
		"Longitude",
		"CreatedAt",
	})

	table.SetColumnAlignment([]int{
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_CENTER,
	})

	for _, node := range nodes {
		table.Append([]string{
			node.Id,
			fmt.Sprintf("%d", node.Participants),
			node.Domain,
			node.IP,
			node.Country,
			fmt.Sprintf("%f", node.Latitude),
			fmt.Sprintf("%f", node.Longitude),
			node.CreatedAt.Format(time.RFC3339),
		})
	}

	table.Render()
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

func helpVerbose(c *cli.Context) error {
	generatedFlags, err := config.GenerateCLIFlags(baseFlags, false)
	if err != nil {
		return err
	}

	c.App.Flags = append(baseFlags, generatedFlags...)
	return cli.ShowAppHelp(c)
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
		grant.Recorder = true
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

func relevantNode(c *cli.Context) error {
	conf, err := getConfig(c)
	if err != nil {
		return errors.Wrap(err, "get config")
	}

	databaseConfig := service.GetDatabaseConfiguration(conf)
	db, err := service.CreateMainDatabaseP2P(databaseConfig, conf)
	if err != nil {
		return errors.Wrap(err, "connect main db")
	}
	geoIp, err := geoip2.FromBytes(livekit2.MixmindDatabase)
	if err != nil {
		return errors.Wrap(err, "create mixmind")
	}
	nodeProvider := service.CreateNodeProvider(geoIp, conf, db)

	clientIP := c.String("client-ip")

	fmt.Printf("Search relevant node for %s\r\n", clientIP)
	n, err := nodeProvider.FetchRelevant(context.Background(), clientIP)
	if err != nil {
		return errors.Wrap(err, "fetch relevant")
	}

	fmt.Printf("Relevant node is %v\r\n", n)
	return nil
}
