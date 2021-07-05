# LiveKit - Open source, distributed video/audio rooms over WebRTC

LiveKit is an open source project that provides scalable, multi-user conferencing over WebRTC. It's designed to give you everything you need to build real time video/audio capabilities in your applications.

## Features

- Horizontally scalable WebRTC Selective Forwarding Unit (SFU)
- Modern, full-featured [client SDKs](https://docs.livekit.io/references/client-sdks/) for JS, iOS, Android
- Built for production - JWT authentication and [server APIs](https://docs.livekit.io/guides/server-api)
- Robust networking & connectivity, over UDP & TCP
- Easy to deploy, a single binary and only three ports to forward.
- Advanced features - speaker detection, simulcasting, selective subscription, moderation APIs.

## Documentation & Guides

Docs & Guides at: https://docs.livekit.io

## Try it live

Head to [our playground](https://livekit.io/playground) and give it a spin. Build a Zoom-like conferencing app in under 100 lines of code!

## SDKs & Tools

Client SDKs:

- [Javascript](https://github.com/livekit/client-sdk-js) ([docs](https://docs.livekit.io/client-sdk-js/))
- [iOS - Swift](https://github.com/livekit/client-sdk-ios) ([docs](https://docs.livekit.io/client-sdk-ios/))
- [Android - Kotlin](https://github.com/livekit/client-sdk-android) ([docs](https://docs.livekit.io/client-sdk-android/))
- [React](https://github.com/livekit/livekit-react)

Server SDKs:

- [Javascript](https://github.com/livekit/server-sdk-js) ([docs](https://docs.livekit.io/server-sdk-js/))
- [Go](https://github.com/livekit/server-sdk-go) ([docs](https://pkg.go.dev/github.com/livekit/server-sdk-go))

Tools:

- [livekit-cli](https://github.com/livekit/livekit-cli)
- [chrometester](https://github.com/livekit/chrometester)

## Installing

### From source

Pre-requisites:

* Go 1.15+ is installed
* GOPATH/bin is in your PATH
* [protoc](https://grpc.io/docs/protoc-installation/) is installed and in PATH

Then run

```shell
git clone https://github.com/livekit/livekit-server
cd livekit-server
./bootstrap.sh
mage
```

### Docker

LiveKit is published to Docker Hub under [livekit/livekit-server](https://hub.docker.com/r/livekit/livekit-server)

## Running

### Creating API keys

LiveKit utilizes JWT based access tokens for authentication to all of its APIs.
Because of this, the server needs a list of valid API keys and secrets to validate the provided tokens. For more, see [Access Tokens guide](https://docs.livekit.io/guides/access-tokens).

Generate API key/secret pairs with:

```shell
./bin/livekit-server generate-keys
```

or

```shell
docker run --rm livekit/livekit-server generate-keys
```

Store the generate keys in a YAML file like:

```yaml
APIwLeah7g4fuLYDYAJeaKsSE: 8nTlwISkb-63DPP7OH4e.nw.J44JjicvZDiz8J59EoQ+
```

### Starting the server

In development mode, LiveKit has no external dependencies. With the key file ready, you can start LiveKit with

```shell
./bin/livekit-server --key-file <path/to/keyfile> --dev
```

or

```shell
docker run --rm -e LIVEKIT_KEYS="<key>: <secret>" livekit/livekit-server --dev
```

the `--dev` flag turns on log verbosity to make it easier for local debugging/development

### Sample client

To test your server, you can use our [example web client](https://example.livekit.io/) 
(built with our [React component](https://github.com/livekit/livekit-react))

Enter generated access token and you are connected to a room! 

### Creating a JWT token

To create a join token for clients, livekit-server provides a convenient subcommand to create a **development** token.
This token has an expiration of a month, which is useful for development & testing, but not appropriate for production use.

```shell
./bin/livekit-server --key-file <path/to/keyfile> create-join-token --room "myroom" --identity "myidentity"
```


## Deploying for production

LiveKit is deployable to any environment that supports docker, including Kubernetes and Amazon ECS.

See deployment docs at https://docs.livekit.io/guides/deploy

## Contributing

We welcome your contributions to make LiveKit better! Please join us [on Slack](https://join.slack.com/t/livekit-users/shared_invite/zt-rrdy5abr-5pZ1wW8pXEkiQxBzFiXPUg) to discuss your ideas and/or submit PRs.

## License

LiveKit server is licensed under Apache License v2.0.
