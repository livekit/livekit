# livekit-server

## Building

Ensure Go 1.14+ is installed, and GOPATH/bin is in your PATH.

Run `make proto && make

## CLI

A CLI is provided to make debugging & testing easier. One of the utilities is the ability to publish tracks from static files.

To use the publish client, download the following files to your computer:
* [happier.ivf](https://www.dropbox.com/s/4ze93d6070s0qj7/happier.ivf?dl=0) - audio track in VP8
* [happier.ogg](https://www.dropbox.com/s/istrnolnh7avftq/happier.ogg?dl=0) - audio track in ogg

To run a peer publishing to a room, do the following:

1. Ensure server is running in dev mode
    ```
    ./bin/livekit-server --dev
    ```

2. Create a room

    ```
    ./bin/livekit-cli create-room
    ```
   
   It'll print out something like this. note the room id

   ```json
      {
        "sid": "RM_CkjigXb6oZQyZ4JNFZqBen",
        "node_ip": "98.35.19.21",
        "creation_time": 1607240104,
        "token": "b9e8c9f6-fbb3-46d5-b6fc-5517186510a6"
      }
   ```

3. Join room as publishing client

   ```
   ./ bin/livekit-cli join --audio <path/to/ogg> --video <path/to/ivf> --room-id <room_sid>
   ```

That's it, join the room with another peer id and see it receiving those tracks

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
