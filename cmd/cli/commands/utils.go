package commands

import (
	"encoding/json"
	"fmt"

	"github.com/urfave/cli/v2"
)

var (
	roomFlag = &cli.StringFlag{
		Name:     "room-id",
		Required: true,
	}
	roomHostFlag = &cli.StringFlag{
		Name:  "host",
		Value: "http://localhost:7880",
	}
	rtcHostFlag = &cli.StringFlag{
		Name:  "host",
		Value: "ws://localhost:7881",
	}
)

func PrintJSON(obj interface{}) {
	txt, _ := json.Marshal(obj)
	fmt.Println(string(txt))
}
