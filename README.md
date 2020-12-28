# livekit-server

## Building

Ensure Go 1.15+ is installed, and GOPATH/bin is in your PATH.

Run `./bootstrap.sh` to install mage, then `mage` to build the project

## Creating API keys

LiveKit utilizes JWT based access tokens to ensure secure access to the APIs and Rooms.
Because of this, the server needs a list of valid API keys and secrets to validate the provided tokens.

Generate API key/secret pairs with:

```
./bin/livekit-server generate-keys
```

Store the generate keys in a YAML file like:

```yaml
APIwLeah7g4fuLYDYAJeaKsSE: 8nTlwISkb-63DPP7OH4e.nw.J44JjicvZDiz8J59EoQ+
...
```

## Starting the server

With the key file ready, you can start LiveKit with

```
./bin/livekit-server --key-file <path/to/keyfile>
```

## CLI

The CLI provides a few tools that makes working with LiveKit easier:
* Room creation/deletion/etc
* Access token creation
* Joining room as a participant
* Publishing files as tracks to a room

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
./bin/livekit-cli create-token --join --r myroom --p <participant name>
```

### Joining a participant

```
./bin/livekit-cli join --token <token>
```

### Publishing static files as tracks

To use the publish client, download the following files to your computer:
* [happier.ivf](https://www.dropbox.com/s/4ze93d6070s0qj7/happier.ivf?dl=0) - audio track in VP8
* [happier.ogg](https://www.dropbox.com/s/istrnolnh7avftq/happier.ogg?dl=0) - audio track in ogg

Join as a publishing participant

```
./ bin/livekit-cli join --audio <path/to/ogg> --video <path/to/ivf> --token <token>
```

That's it, join the room with another participant and see it receiving those tracks

## Protocol

LiveKit provides room based audio/video/data channels based on WebRTC.
It provides a set of APIs to manipulate rooms, as well as its own signaling protocol to exchange room and participant information.

Room APIs are defined in room.proto, it's fairly straight forward with typical CRUD APIs. Room APIs are HTTP, built with Twirp and follows [its the conventions](https://twitchtv.github.io/twirp/docs/routing.html).

The RTC service provides the signaling and everything else when the client interacts with the room. RTC service requires bidirectional
communication between the client and server, and exchanges messages via WebSocket. Messages are encoded in either JSON or binary protobuf,
see `rtc.proto` for the message structure. 

The flow for interaction is:
1. Establish WebSocket to ws://<host>:<port>/rtc
1. Server will send back a `SignalResponse` with a `join` response. It'll include the new participant's details, and what other participants are in the room
1. Client sends a `SignalRequest` with an WebRTC `offer`
1. Server will send back a `SignalResponse` with an `answer`
1. Client and server will exchange ice candidates via `trickle` in the request & responses
