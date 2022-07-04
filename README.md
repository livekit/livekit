# LiveKit - Open source, high performance WebRTC infrastructure

LiveKit is an open source project that provides scalable, multi-user conferencing over WebRTC. It's designed to give you
everything you need to build real time video/audio/data capabilities in your applications.

LiveKit is written in Go, using the awesome [Pion WebRTC](https://github.com/pion/webrtc) implementation.

## Features

- Scalable, distributed WebRTC SFU (Selective Forwarding Unit)
- Modern, full-featured [client SDKs](https://docs.livekit.io/references/client-sdks/)
- Built for production - JWT authentication and [server APIs](https://docs.livekit.io/guides/server-api)
- Robust networking & connectivity. UDP/TCP/TURN
- Easy to deploy - single binary, docker & kubernetes
- Advanced features - speaker detection, simulcast, selective subscription, moderation APIs, and webhooks.

## Documentation & Guides

Docs & Guides at: https://docs.livekit.io

## Try it live

Head to [our playground](https://livekit.io/playground) and give it a spin. Build a Zoom-like conferencing app in under
100 lines of code!

## SDKs & Tools

Client SDKs:

- [JavaScript](https://github.com/livekit/client-sdk-js) ([docs](https://docs.livekit.io/client-sdk-js/))
- [React](https://github.com/livekit/livekit-react)
- [iOS & MacOS - Swift](https://github.com/livekit/client-sdk-swift) ([docs](https://docs.livekit.io/client-sdk-swift/))
- [Android - Kotlin](https://github.com/livekit/client-sdk-android) ([docs](https://docs.livekit.io/client-sdk-android/))
- [Flutter](https://github.com/livekit/client-sdk-flutter) ([docs](https://docs.livekit.io/client-sdk-flutter/))
- [Unity (WebGL)](https://github.com/livekit/client-sdk-unity-web) ([docs](https://livekit.github.io/client-sdk-unity-web/) [demo](https://unity.livekit.io))
- [React Native](https://github.com/livekit/client-sdk-react-native)

Server SDKs:

- [JavaScript](https://github.com/livekit/server-sdk-js) ([docs](https://docs.livekit.io/server-sdk-js/))
- [Go](https://github.com/livekit/server-sdk-go) ([docs](https://pkg.go.dev/github.com/livekit/server-sdk-go))
- [Ruby](https://github.com/livekit/server-sdk-ruby)
- [Python (community)](https://github.com/tradablebits/livekit-server-sdk-python)
- [PHP (community)](https://github.com/agence104/livekit-server-sdk-php)

Tools:

- [Egress](https://github.com/livekit/egress): export and record your rooms
- [livekit-cli](https://github.com/livekit/livekit-cli): command line admin & tools
- [livekit-load-tester](https://github.com/livekit/livekit-cli#livekit-load-tester): load testing
- [Docker image](https://hub.docker.com/r/livekit/livekit-server)
- [Helm charts](https://github.com/livekit/livekit-helm)

## Quickstart

### Generate config file and keys

```shell
docker run --rm -v$PWD:/output livekit/generate --local
```

The above command generates a `livekit.yaml` you can use to start LiveKit. It'll contain an API key/secret pair you can
use with your LiveKit install.

### Starting with docker

```shell
docker run --rm -p 7880:7880 \
    -p 7881:7881 \
    -p 7882:7882/udp \
    -v $PWD/livekit.yaml:/livekit.yaml \
    livekit/livekit-server \
    --config /livekit.yaml \
    --node-ip <machine-ip>
```

When running with docker, `--node-ip` needs to be set to your machine's IP address. If the service will be exposed to
the public Internet, this should the machine's public IP.

### Test with example app

Head over to the [example app](https://example.livekit.io) and enter the generated token to connect to your LiveKit
server. This app is built with our [React SDK](https://github.com/livekit/livekit-react).

Once connected, your video and audio are now published to your new LiveKit instance!

### Generating access tokens (JWT)

To add more users to a room, you'll have to create a token for each
participant. [Learn more about access tokens](https://docs.livekit.io/guides/access-tokens/).

`livekit-server` provides a convenient sub-command to create a development token. This token has an expiration of a
month, which is useful for development and testing, but not appropriate for production use.

```shell
docker run --rm -e LIVEKIT_KEYS="<api-key>: <api-secret>" \
    livekit/livekit-server create-join-token \
    --room "<room-name>" \
    --identity "<participant-identity>"
```

## Deploying to server

Deployment Docs: https://docs.livekit.io/deploy/

### Single node server

Use our deploy config generator to set up a single node deployment with automatic TLS termination and built-in TURN.

It includes a cloud-init/setup script that's supported by most cloud environments.

```shell
docker run --rm -it -v$PWD:/output livekit/generate
```

### Kubernetes

We publish a [helm chart](https://github.com/livekit/livekit-helm) that helps you to set up a cluster with high
availability. For detailed instructions, see [Kubernetes guide](https://docs.livekit.io/deploy/kubernetes)

### Testing your deployment

Use the [connection tester](https://livekit.io/connection-test) to ensure your installation is set up properly for user
traffic.

## Building from source

Pre-requisites:

* Go 1.15+ is installed
* GOPATH/bin is in your PATH

Then run

```shell
git clone https://github.com/livekit/livekit
cd livekit
./bootstrap.sh
mage
```

## Contributing

We welcome your contributions to make LiveKit better! Please join us
[on Slack](http://livekit.io/join-slack) to discuss your ideas and/or
submit PRs.

## License

LiveKit server is licensed under Apache License v2.0.
