# LiveKit - Open source, distributed video/audio rooms based on WebRTC

## Building

Ensure Go 1.15+ is installed, and GOPATH/bin is in your PATH.

Run `./bootstrap.sh` to install mage, then `mage` to build the project

## Creating API keys

LiveKit utilizes JWT based access tokens for authentication to all of its APIs.
Because of this, the server needs a list of valid API keys and secrets to validate the provided tokens. For more, see [Authentication](docs/authentication.md).

Generate API key/secret pairs with:

```
./bin/livekit-server generate-keys
```

Store the generate keys in a YAML file like:

```yaml
APIwLeah7g4fuLYDYAJeaKsSE: 8nTlwISkb-63DPP7OH4e.nw.J44JjicvZDiz8J59EoQ+
```

## Running the server

LiveKit server is a distributed service that is architected for scale. It's self-encapsulated in a single binary, and horizontally scalable by adding more instances. In a production setup, the only external dependency is Redis. LiveKit uses Redis to store room & participant information, and utilizes its pub/sub functionality to coordinate activity between instances.

### Running for development

In development mode, LiveKit has no external dependencies. With the key file ready, you can start LiveKit with

```
./bin/livekit-server --key-file <path/to/keyfile> --dev
```

the `--dev` flag turns on log verbosity to make it easier for local debugging/development

### Running for production

TBD

## CLI

The CLI provides a few tools that makes working with LiveKit easier:

- Room creation/deletion/etc
- Access token creation
- Joining room as a participant
- Publishing files as tracks to a room

### Setting API key and secret

CLI commands require --api-key and --api-secret arguments. To avoid having to pass these in for each command, it's simplest to set them as environment vars:

```bash
export LK_API_KEY=<key>
export LK_API_SECRET=<secret>
```

### Creating a room

```
./bin/livekit-cli create-room --name myroom
```

### Creating a participant token

```
./bin/livekit-cli create-token --join --r myroom --dev --p <participant name>
```

`--dev` creates a development token that doesn't expire for a year

### Joining a participant

```
./bin/livekit-cli join --token <token>
```

### Publishing static files as tracks

To use the publish client, download the following files to your computer:

- [happier.ivf](https://www.dropbox.com/s/4ze93d6070s0qj7/happier.ivf?dl=0) - video track in VP8
- [happier.ogg](https://www.dropbox.com/s/istrnolnh7avftq/happier.ogg?dl=0) - audio track in ogg

Join as a publishing participant

```
./ bin/livekit-cli join --audio <path/to/ogg> --video <path/to/ivf> --token <token>
```

That's it, join the room with another participant and see it receiving those tracks

## APIs & Protocol

`livekit-server` provides two primary services, a `Room` service that allows for room creation & management, and `RTC` service to handle real time communications.

Room APIs are defined in [room.proto](proto/room.proto), it's fairly straight forward with typical CRUD APIs. Room APIs are in HTTP, built with Twirp and follows [its the conventions](https://twitchtv.github.io/twirp/docs/routing.html).

The RTC service provides the signaling and everything else when the client interacts with the room. See [RTC Protocol](docs/protocol.md).
