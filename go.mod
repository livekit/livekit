module github.com/livekit/livekit-server

go 1.15

require (
	github.com/bep/debounce v1.2.0
	github.com/go-redis/redis/v8 v8.4.8
	github.com/golang/protobuf v1.4.3
	github.com/google/wire v0.4.0
	github.com/gorilla/websocket v1.4.2
	github.com/karlseguin/ccache/v2 v2.0.7
	github.com/lithammer/shortuuid/v3 v3.0.4
	github.com/lytics/base62 v0.0.0-20180808010106-0ee4de5a5d6d
	github.com/magefile/mage v1.10.0
	github.com/maxbrunsfeld/counterfeiter/v6 v6.3.0
	github.com/mitchellh/go-homedir v1.1.0
	github.com/pion/ion-sfu v1.9.0
	github.com/pion/rtcp v1.2.6
	github.com/pion/rtp v1.6.2
	github.com/pion/sdp/v3 v3.0.4
	github.com/pion/stun v0.3.5
	github.com/pion/webrtc/v3 v3.0.11
	github.com/pkg/errors v0.9.1
	github.com/stretchr/testify v1.7.0
	github.com/thoas/go-funk v0.7.0
	github.com/twitchtv/twirp v7.1.0+incompatible
	github.com/urfave/cli/v2 v2.2.0
	github.com/urfave/negroni v1.0.0
	go.uber.org/zap v1.16.0
	golang.org/x/mod v0.4.0 // indirect
	golang.org/x/tools v0.0.0-20201222163215-f2e330f49058 // indirect
	google.golang.org/protobuf v1.25.0
	gopkg.in/square/go-jose.v2 v2.5.1
	gopkg.in/yaml.v3 v3.0.0-20200615113413-eeeca48fe776
)

replace github.com/pion/ion-sfu => github.com/davidzhao/ion-sfu v1.8.3-0.20210221051003-77fda1d89698
