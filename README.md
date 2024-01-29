# dTelecom Cloud: Decentralized Live Video SDK

[dTelecom](https://dtelecom.org) is an open-source communication infrastructure enabling audio/video conferencing and live streaming using the WebRTC technology. It's crafted to deliver all necessary components to integrate real-time video and audio functionalities into your applications.

dTelecom's server is written in Go, using the awesome [Pion WebRTC](https://github.com/pion/webrtc) implementation.

## MacOS
```shell
sudo sysctl -w net.inet.udp.recvspace=2500000
```

## Features

- Scalable, distributed WebRTC SFU (Selective Forwarding Unit)
- Modern, full-featured client SDKs
- Built for production, supports JWT authentication
- Robust networking and connectivity, UDP/TCP/TURN
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


### Use dTelecom Cloud

dTelecom Cloud is the fastest and most reliable way to run dTelecom. Every project gets free monthly bandwidth credits.

Sign up for [dTelecom Cloud](https://cloud.dtelecom.org/).

## Building from source

Pre-requisites:

- Go 1.18+ is installed
- GOPATH/bin is in your PATH

Then run

```shell
git clone https://github.com/dTelecom/livekit
cd livekit
./bootstrap.sh
mage
```

## Contributing

We welcome your contributions toward improving dTelecom! Please [join us](http://dtelecom.org) to discuss your ideas and/or PRs.

## License

dTelecom server is licensed under Apache License v2.0.
