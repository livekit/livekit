# LiveKit - Open source, distributed video/audio rooms over WebRTC

LiveKit is an open source project that provides scalable, multi-user conferencing over WebRTC. It's designed to give you everything you need to build real time video/audio capabilities in your applications.

## Features

- Horizontally scalable WebRTC Selective Forwarding Unit (SFU)
- Modern, full-featured [client SDKs](references/client-sdks.md) for JS, iOS, Android
- Built for production - JWT authentication and [server APIs](references/server-apis.md)
- Robust networking & connectivity, over UDP & TCP
- Easy to deploy, a single binary and only three ports to forward.
- Advanced features - simulcasting, selective subscription, moderation APIs.

## Documentation & Guides

Docs & Guides at: https://docs.livekit.io

## SDKs & APIs

Client SDKs:

- [Javascript](https://github.com/livekit/client-sdk-js) ([docs](https://docs.livekit.io/client-sdk-js/))
- [iOS - Swift](https://github.com/livekit/client-sdk-ios) ([docs](https://docs.livekit.io/client-sdk-ios/))
- [Android - Kotlin](https://github.com/livekit/client-sdk-android) ([docs](https://docs.livekit.io/client-sdk-android/))

Server APIs:

- [Javascript](https://github.com/livekit/server-api-js) ([docs](https://docs.livekit.io/server-api-js/))
- [Go](https://github.com/livekit/livekit-sdk-go) ([docs](https://pkg.go.dev/github.com/livekit/livekit-sdk-go))

## Installing

### From source

Ensure Go 1.15+ is installed, and GOPATH/bin is in your PATH.

```shell
git clone https://github.com/livekit/livekit-server
cd livekit-server
./bootstrap.sh
mage
```

### Docker

LiveKit is published to dockerhub under livekit/livekit-server

## Running

### Creating API keys

LiveKit utilizes JWT based access tokens for authentication to all of its APIs.
Because of this, the server needs a list of valid API keys and secrets to validate the provided tokens. For more, see [Authentication](docs/authentication.md).

Generate API key/secret pairs with:

```shell
./bin/livekit-server generate-keys
```

or

```shell
docker run livekit/livekit-server:v0.9 generate-keys
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
docker run -e LIVEKIT_KEYS="<key>: <secret>" livekit/livekit-server:v0.9 --dev
```

the `--dev` flag turns on log verbosity to make it easier for local debugging/development

### Sample client

[client-sdk-js](https://github.com/livekit/client-sdk-js) contains a web sample client that can connect to your server locally.

Clone the repo and run `yarn sample`

### Creating a JWT token

To create a join token for clients, livekit-server provides a convenient subcommand to create a **development** token.
This token has an expiration of a month, which is useful for development & testing, but not appropriate for production use.

```shell
./bin/livekit-server --key-file <path/to/keyfile> create-join-token --room "myroom" --identity "myidentity"
```


## Deploying for production

LiveKit is deployable to any environment that supports docker, including Kubernetes and Amazon ECS

See documentation at https://docs.livekit.io/guides/deploy

## License

LiveKit server is licensed under AGPL v3.0.

## APIs & Protocol

`livekit-server` provides two primary services, a `Room` service that allows for room management and participant moderation, and `RTC` service to handle WebRTC communications.

Room APIs are defined in [room.proto](proto/room.proto). Room APIs are in HTTP, built with Twirp and follows [its the conventions](https://twitchtv.github.io/twirp/docs/routing.html).

The RTC service provides the signaling and everything else when the client interacts with the room.
