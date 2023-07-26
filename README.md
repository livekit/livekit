# dTelecom Cloud: Decentralized Live Video SDK

[dTelecom](https://dtelecom.org) is an open-source communication infrastructure enabling audio/video conferencing and live streaming using the WebRTC technology. It's crafted to deliver all necessary components to integrate real-time video and audio functionalities into your applications.

dTelecom's server is written in Go, using the awesome [Pion WebRTC](https://github.com/pion/webrtc) implementation.

## Features

- Scalable, distributed WebRTC SFU (Selective Forwarding Unit)
- Modern, full-featured client SDKs
- Built for production, supports JWT authentication
- Robust networking and connectivity, UDP/TCP/TURN
- Easy to deploy: single binary, Docker or Kubernetes
- Advanced features including:
    - speaker detection
    - simulcast
    - end-to-end optimizations
    - selective subscription
    - moderation APIs
    - webhooks
    - distributed and multi-region

## Documentation & Guides

https://docs.dtelecom.org

## Live Demos

- [Web3 Meeting](https://dmeet.org) ([source](https://github.com/dTelecom/conference-example))
- [Web3 Spatial Room](https://spatial.dmeet.org) ([source](https://github.com/dTelecom/spatial-audio))

## SDKs & Tools

### Client SDKs

Client SDKs enable your frontend to include interactive, multi-user experiences.

<table>
  <tr>
    <th>Language</th>
    <th>Repo</th>
    <th>
        <a href="https://docs.dtelecom.org/guides/room/events/#declarative-ui" target="_blank" rel="noopener noreferrer">Declarative UI</a>
    </th>
    <th>Links</th>
  </tr>
  <!-- BEGIN Template
  <tr>
    <td>Language</td>
    <td>
      <a href="" target="_blank" rel="noopener noreferrer"></a>
    </td>
    <td></td>
    <td></td>
  </tr>
  END -->
  <!-- JavaScript -->
  <tr>
    <td>JavaScript (TypeScript)</td>
    <td>
      <a href="https://github.com/livekit/client-sdk-js" target="_blank" rel="noopener noreferrer">client-sdk-js</a>
    </td>
    <td>
      <a href="https://github.com/livekit/livekit-react" target="_blank" rel="noopener noreferrer">React</a>
    </td>
    <td>
      <a href="https://docs.livekit.io/client-sdk-js/" target="_blank" rel="noopener noreferrer">docs</a>
      |
      <a href="https://github.com/livekit/client-sdk-js/tree/main/example" target="_blank" rel="noopener noreferrer">JS example</a>
      |
      <a href="https://github.com/livekit/client-sdk-js/tree/main/example" target="_blank" rel="noopener noreferrer">React example</a>
    </td>
  </tr>
</table>

### Server SDKs

Server SDKs enable your backend to generate [access tokens](https://docs.livekit.io/guides/access-tokens/),
call [server APIs](https://docs.livekit.io/guides/server-api/), and
receive [webhooks](https://docs.livekit.io/guides/webhooks/). In addition, the Go SDK includes client capabilities,
enabling you to build automations that behave like end-users.

| Language                | Repo                                                                                                | Docs                                                        |
|:------------------------|:----------------------------------------------------------------------------------------------------|:------------------------------------------------------------|
| JavaScript (TypeScript) | [server-sdk-js](https://github.com/livekit/server-sdk-js)                                           | [docs](https://docs.livekit.io/server-sdk-js/)              |


### Ecosystem & Tools

- [CLI](https://github.com/livekit/livekit-cli) - command line interface & load tester
- [Egress](https://github.com/livekit/egress) - export and record your rooms
- [Ingress](https://github.com/livekit/ingress) - ingest streams from RTMP / OBS Studio
- [Docker image](https://hub.docker.com/r/livekit/livekit-server)
- [Helm charts](https://github.com/livekit/livekit-helm)

## Install

We recommend installing [livekit-cli](https://github.com/livekit/livekit-cli) along with the server. It lets you access
server APIs, create tokens, and generate test traffic.

### MacOS

```shell
brew install livekit
```

### Linux

```shell
curl -sSL https://get.livekit.io | bash
```

### Windows

Download the [latest release here](https://github.com/livekit/livekit/releases/latest)

## Getting Started

### Starting LiveKit

Start LiveKit in development mode by running `livekit-server --dev`. It'll use a placeholder API key/secret pair.

```
API Key: devkey
API Secret: secret
```

To customize your setup for production, refer to our [deployment docs](https://docs.livekit.io/deploy/)

### Creating access token

A user connecting to a LiveKit room requires an [access token](https://docs.livekit.io/guides/access-tokens/). Access
tokens (JWT) encode the user's identity and the room permissions they've been granted. You can generate a token with our
CLI:

```shell
livekit-cli create-token \
    --api-key devkey --api-secret secret \
    --join --room my-first-room --identity user1 \
    --valid-for 24h
```

### Test with example app

Head over to our [example app](https://example.livekit.io) and enter a generated token to connect to your LiveKit
server. This app is built with our [React SDK](https://github.com/livekit/livekit-react).

Once connected, your video and audio are now being published to your new LiveKit instance!

### Simulating a test publisher

```shell
livekit-cli join-room \
    --url ws://localhost:7880 \
    --api-key devkey --api-secret secret \
    --room my-first-room --identity bot-user1 \
    --publish-demo
```

This command publishes a looped demo video to a room. Due to how the video clip was encoded (keyframes every 3s),
there's a slight delay before the browser has sufficient data to begin rendering frames. This is an artifact of the
simulation.

## Deployment

### Use dTelecom Cloud

dTelecom Cloud is the fastest and most reliable way to run dTelecom. Every project gets free monthly bandwidth credits.

Sign up for [dTelecom Cloud](https://cloud.dtelecom.org/).

### Self-host

Read our [deployment docs](https://docs.livekit.io/deploy/) for more information.

## Building from source

Pre-requisites:

- Go 1.18+ is installed
- GOPATH/bin is in your PATH

Then run

```shell
git clone https://github.com/livekit/livekit
cd livekit
./bootstrap.sh
mage
```

## Contributing

We welcome your contributions toward improving dTelecom! Please [join us](http://dtelecom.org) to discuss your ideas and/or PRs.

## License

dTelecom server is licensed under Apache License v2.0.
