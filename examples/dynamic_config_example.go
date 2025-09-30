package main

import (
	"fmt"

	"github.com/livekit/livekit-server/pkg/clientconfiguration"
	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/protocol/livekit"
)

func main() {
	// Example configuration
	conf := &config.ClientConfigurationConfig{
		Configurations: []config.ClientConfigurationItem{
			{
				Match:         `c.browser == "safari"`,
				Configuration: `{"disabled_codecs": {"codecs": [{"mime": "video/AV1"}]}}`,
				Merge:         true,
			},
			{
				Match:         `c.os == "android"`,
				Configuration: `{"video": {"hardware_encoder": 1}}`,
				Merge:         true,
			},
		},
	}

	// Create dynamic configuration manager
	manager, err := clientconfiguration.NewDynamicClientConfigurationManager(conf)
	if err != nil {
		panic(err)
	}

	// Test Safari client
	safariClient := &livekit.ClientInfo{
		Browser: "safari",
		Version: "16.0",
	}
	
	config := manager.GetConfiguration(safariClient)
	fmt.Printf("Safari client configuration: %+v\n", config)

	// Test Android client
	androidClient := &livekit.ClientInfo{
		Os: "android",
	}
	
	config = manager.GetConfiguration(androidClient)
	fmt.Printf("Android client configuration: %+v\n", config)

	// Test Chrome client (should get static config only)
	chromeClient := &livekit.ClientInfo{
		Browser: "chrome",
		Version: "120.0",
	}
	
	config = manager.GetConfiguration(chromeClient)
	fmt.Printf("Chrome client configuration: %+v\n", config)

	fmt.Println("Dynamic client configuration test completed successfully!")
}
