package main

import (
	"fmt"

	"github.com/livekit/livekit-server/pkg/node"
)

func main() {
	n, err := node.NewLocalNode()
	if err != nil {
		panic(err)
	}

	if err := n.DiscoverNetworkInfo(); err != nil {
		panic(err)
	}

	fmt.Println("my IP", n.Ip)
}
