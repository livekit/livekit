package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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

func ExpandUser(p string) string {
	if strings.HasPrefix(p, "~") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[1:])
	}

	return p
}
