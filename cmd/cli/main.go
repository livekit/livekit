package main

import (
	"fmt"
	"os"

	"github.com/urfave/cli/v2"

	"github.com/livekit/livekit-server/cmd/cli/commands"
	"github.com/livekit/livekit-server/pkg/logger"
)

// command line util that tests server
func main() {
	app := &cli.App{
		Name: "livekit-cli",
	}

	app.Commands = append(app.Commands, commands.RoomCommands...)
	app.Commands = append(app.Commands, commands.RTCCommands...)
	app.Commands = append(app.Commands, commands.TokenCommands...)

	logger.InitDevelopment("")
	if err := app.Run(os.Args); err != nil {
		fmt.Println(err)
	}
}
