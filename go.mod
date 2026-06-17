module github.com/livekit/livekit-server

go 1.26

require (
	github.com/bep/debounce v1.2.1
	github.com/d5/tengo/v2 v2.17.0
	github.com/dennwc/iters v1.2.2
	github.com/dustin/go-humanize v1.0.1
	github.com/elliotchance/orderedmap/v3 v3.1.0
	github.com/florianl/go-tc v0.4.8
	github.com/frostbyte73/core v0.1.1
	github.com/gammazero/deque v1.2.1
	github.com/gammazero/workerpool v1.2.1
	github.com/google/uuid v1.6.0
	github.com/google/wire v0.7.0
	github.com/gorilla/websocket v1.5.3
	github.com/hashicorp/go-version v1.9.0
	github.com/hashicorp/golang-lru/v2 v2.0.7
	github.com/jellydator/ttlcache/v3 v3.4.0
	github.com/jxskiss/base62 v1.1.0
	github.com/livekit/mageutil v0.0.0-20250511045019-0f1ff63f7731
	github.com/livekit/mediatransportutil v0.0.0-20260608063931-a3417d38cda0
	github.com/livekit/protocol v1.47.1-0.20260617164816-2f8e4d6d263b
	github.com/livekit/psrpc v0.7.2
	github.com/mackerelio/go-osstat v0.2.7
	github.com/magefile/mage v1.17.2
	github.com/maxbrunsfeld/counterfeiter/v6 v6.12.2
	github.com/mitchellh/go-homedir v1.1.0
	github.com/moby/moby/client v0.4.1
	github.com/olekukonko/tablewriter v1.1.4
	github.com/ory/dockertest/v4 v4.0.0
	github.com/pion/datachannel v1.6.0
	github.com/pion/dtls/v3 v3.1.4
	github.com/pion/ice/v4 v4.2.7
	github.com/pion/interceptor v0.1.45
	github.com/pion/rtcp v1.2.16
	github.com/pion/rtp v1.10.2
	github.com/pion/sctp v1.9.5
	github.com/pion/sdp/v3 v3.0.18
	github.com/pion/transport/v4 v4.0.2
	github.com/pion/turn/v5 v5.0.8
	github.com/pion/webrtc/v4 v4.2.11
	github.com/pkg/errors v0.9.1
	github.com/prometheus/client_golang v1.23.2
	github.com/redis/go-redis/v9 v9.20.0
	github.com/rs/cors v1.11.1
	github.com/stretchr/testify v1.11.1
	github.com/thoas/go-funk v0.9.3
	github.com/tomnomnom/linkheader v0.0.0-20250811210735-e5fe3b51442e
	github.com/twitchtv/twirp v8.1.3+incompatible
	github.com/ua-parser/uap-go v0.0.0-20260529044130-17c35e68e58c
	github.com/urfave/negroni/v3 v3.1.1
	go.uber.org/atomic v1.11.0
	go.uber.org/multierr v1.11.0
	go.uber.org/zap v1.28.0
	golang.org/x/mod v0.36.0
	golang.org/x/sync v0.20.0
	google.golang.org/protobuf v1.36.11
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cilium/ebpf v0.16.0 // indirect
	github.com/clipperhouse/displaywidth v0.11.0 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/containerd/errdefs v1.0.0 // indirect
	github.com/containerd/errdefs/pkg v0.3.0 // indirect
	github.com/distribution/reference v0.6.0 // indirect
	github.com/fatih/color v1.19.0 // indirect
	github.com/felixge/httpsnoop v1.0.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/goccy/go-json v0.10.6 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.29.0 // indirect
	github.com/mattn/go-colorable v0.1.15 // indirect
	github.com/mattn/go-isatty v0.0.22 // indirect
	github.com/moby/moby/api v1.54.2 // indirect
	github.com/nyaruka/phonenumbers v1.8.0 // indirect
	github.com/olekukonko/cat v0.0.0-20250911104152-50322a0618f6 // indirect
	github.com/olekukonko/errors v1.3.0 // indirect
	github.com/olekukonko/ll v0.1.8 // indirect
	github.com/puzpuzpuz/xsync/v4 v4.5.0 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.69.0 // indirect
	go.opentelemetry.io/otel v1.44.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.44.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp v1.44.0 // indirect
	go.opentelemetry.io/otel/metric v1.44.0 // indirect
	go.opentelemetry.io/otel/sdk v1.44.0 // indirect
	go.opentelemetry.io/otel/trace v1.44.0 // indirect
	go.opentelemetry.io/proto/otlp v1.10.0 // indirect
	go.yaml.in/yaml/v3 v3.0.4 // indirect
	golang.org/x/exp v0.0.0-20260603202125-055de637280b // indirect
	golang.org/x/time v0.15.0 // indirect
)

require (
	buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go v1.36.11-20260415201107-50325440f8f2.1 // indirect
	buf.build/go/protovalidate v1.2.0 // indirect
	buf.build/go/protoyaml v0.7.0 // indirect
	cel.dev/expr v0.25.2 // indirect
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/antlr4-go/antlr/v4 v4.13.1 // indirect
	github.com/benbjohnson/clock v1.3.5 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/docker/go-connections v0.7.0 // indirect
	github.com/docker/go-units v0.5.0 // indirect
	github.com/fsnotify/fsnotify v1.10.1 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/google/cel-go v0.28.1 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/google/subcommands v1.2.0 // indirect
	github.com/hashicorp/go-cleanhttp v0.5.2 // indirect
	github.com/hashicorp/go-retryablehttp v0.7.8 // indirect
	github.com/hashicorp/golang-lru v1.0.2 // indirect
	github.com/josharian/native v1.1.0 // indirect
	github.com/klauspost/compress v1.18.6 // indirect
	github.com/klauspost/cpuid/v2 v2.3.0 // indirect
	github.com/lithammer/shortuuid/v4 v4.2.0 // indirect
	github.com/mattn/go-runewidth v0.0.24 // indirect
	github.com/mdlayher/netlink v1.11.2 // indirect
	github.com/mdlayher/socket v0.6.1 // indirect
	github.com/moby/docker-image-spec v1.3.1 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/nats-io/nats.go v1.52.0 // indirect
	github.com/nats-io/nkeys v0.4.16 // indirect
	github.com/nats-io/nuid v1.0.1 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/opencontainers/image-spec v1.1.1 // indirect
	github.com/pion/logging v0.2.4 // indirect
	github.com/pion/mdns/v2 v2.1.0 // indirect
	github.com/pion/randutil v0.1.0 // indirect
	github.com/pion/srtp/v3 v3.0.11 // indirect
	github.com/pion/stun/v3 v3.1.4
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.68.1 // indirect
	github.com/prometheus/procfs v0.20.1 // indirect
	github.com/urfave/cli/v3 v3.9.0
	github.com/wlynxg/anet v0.0.5 // indirect
	github.com/zeebo/xxh3 v1.1.0 // indirect
	go.uber.org/zap/exp v0.3.0 // indirect
	golang.org/x/crypto v0.52.0 // indirect
	golang.org/x/net v0.55.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	golang.org/x/tools v0.45.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	google.golang.org/grpc v1.81.1 // indirect
)
