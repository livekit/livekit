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
    ./bin/livekit-cli create-room --room-id hello
    ```

3. Join room as publishing client

   ```
   ./ bin/livekit-cli join --room-id hello --audio <path/to/ogg> --video <path/to/ivf>
   ```

That's it, join the room with another peer id and see it receiving those tracks
