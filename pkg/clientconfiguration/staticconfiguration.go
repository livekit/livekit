package clientconfiguration

import (
	"fmt"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"google.golang.org/protobuf/proto"
)

type ConfigurationItem struct {
	Match
	Configuration *livekit.RTCClientConfiguration
	Merge         bool
}

type StaticClientConfigurationManager struct {
	confs []ConfigurationItem
}

func NewStaticClientConfigurationManager(confs []ConfigurationItem) *StaticClientConfigurationManager {
	return &StaticClientConfigurationManager{confs: confs}
}

func (s *StaticClientConfigurationManager) GetConfiguration(clientInfo *livekit.ClientInfo) *livekit.RTCClientConfiguration {
	var matchedConf []*livekit.RTCClientConfiguration
	for _, c := range s.confs {
		matched, err := c.Match.Match(clientInfo)
		if err != nil {
			logger.Errorw(fmt.Sprintf("matchrule failed, clientInfo: %s", clientInfo.String()), err)
			continue
		}
		if !matched {
			continue
		}
		if !c.Merge {
			return c.Configuration
		}
		matchedConf = append(matchedConf, c.Configuration)
	}

	var conf *livekit.RTCClientConfiguration
	for k, v := range matchedConf {
		if k == 0 {
			conf = proto.Clone(matchedConf[0]).(*livekit.RTCClientConfiguration)
		} else {
			// TODO : there is a problem use protobuf merge, we don't have flag to indicate 'no value',
			// don't override default behavior or other configuration's field. So a bool value = false or
			// a int value = 0 will override same field in other configuration
			proto.Merge(conf, v)
		}
	}
	return conf
}
