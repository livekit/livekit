module github.com/livekit/livekit-server

go 1.15

require (
	github.com/bep/debounce v1.2.0
	github.com/c9s/goprocinfo v0.0.0-20210130143923-c95fcf8c64a8
	github.com/cpuguy83/go-md2man/v2 v2.0.0 // indirect
	github.com/gammazero/workerpool v1.1.2
	github.com/go-logr/logr v1.1.0
	github.com/go-logr/zapr v1.1.0
	github.com/go-redis/redis/v8 v8.11.3
	github.com/google/subcommands v1.2.0 // indirect
	github.com/google/wire v0.5.0
	github.com/gorilla/websocket v1.4.2
	github.com/jxskiss/base62 v0.0.0-20191017122030-4f11678b909b
	github.com/livekit/protocol v0.9.9
	github.com/magefile/mage v1.11.0
	github.com/maxbrunsfeld/counterfeiter/v6 v6.3.0
	github.com/mitchellh/go-homedir v1.1.0
	github.com/olekukonko/tablewriter v0.0.5
	github.com/pion/ice/v2 v2.1.12
	github.com/pion/interceptor v0.1.0
	github.com/pion/ion-sfu v1.10.5
	github.com/pion/logging v0.2.2
	github.com/pion/rtcp v1.2.8
	github.com/pion/rtp v1.7.2
	github.com/pion/sdp/v3 v3.0.4
	github.com/pion/stun v0.3.5
	github.com/pion/transport v0.12.3
	github.com/pion/turn/v2 v2.0.5
	github.com/pion/webrtc/v3 v3.1.5
	github.com/pkg/errors v0.9.1
	github.com/prometheus/client_golang v1.10.0
	github.com/prometheus/common v0.19.0 // indirect
	github.com/russross/blackfriday/v2 v2.1.0 // indirect
	github.com/stretchr/testify v1.7.0
	github.com/thoas/go-funk v0.8.0
	github.com/twitchtv/twirp v8.1.0+incompatible
	github.com/urfave/cli/v2 v2.3.0
	github.com/urfave/negroni v1.0.0
	go.uber.org/zap v1.19.1
	golang.org/x/mod v0.5.1 // indirect
	golang.org/x/sys v0.0.0-20211007075335-d3039528d8ac // indirect
	golang.org/x/tools v0.1.7 // indirect
	google.golang.org/protobuf v1.27.1
	gopkg.in/yaml.v3 v3.0.0-20210107192922-496545a6307b
)

replace github.com/pion/ion-sfu => github.com/livekit/ion-sfu v1.20.13
