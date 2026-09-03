<!--BEGIN_BANNER_IMAGE-->

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="/.github/banner_dark.png">
  <source media="(prefers-color-scheme: light)" srcset="/.github/banner_light.png">
  <img style="width:100%;" alt="The LiveKit icon, the name of the repository and some sample code in the background." src="https://raw.githubusercontent.com/livekit/livekit/main/.github/banner_light.png">
</picture>

<!--END_BANNER_IMAGE-->

# LiveKit: Realtime infrastructure for voice, video, and AI agents

[LiveKit](https://livekit.com) is an open source platform for building voice, video, and physical AI agents.
This repository is the LiveKit server: a scalable, distributed WebRTC SFU that moves realtime audio, video, and
data between people, devices, and AI models. The SDKs, agents frameworks, and companion services are linked in
the table at the bottom of this page.

LiveKit's server is written in Go, using the awesome [Pion WebRTC](https://github.com/pion/webrtc) implementation.

[![GitHub stars](https://img.shields.io/github/stars/livekit/livekit?style=social&label=Star&maxAge=2592000)](https://github.com/livekit/livekit/stargazers/)
[![Slack community](https://img.shields.io/endpoint?url=https%3A%2F%2Flivekit.io%2Fbadges%2Fslack)](https://livekit.io/join-slack)
[![Twitter Follow](https://img.shields.io/twitter/follow/livekit)](https://twitter.com/livekit)
[![Ask DeepWiki](https://deepwiki.com/badge.svg)](https://deepwiki.com/livekit/livekit)
[![GitHub release (latest SemVer)](https://img.shields.io/github/v/release/livekit/livekit)](https://github.com/livekit/livekit/releases/latest)
[![GitHub Workflow Status](https://img.shields.io/github/actions/workflow/status/livekit/livekit/buildtest.yaml?branch=master)](https://github.com/livekit/livekit/actions/workflows/buildtest.yaml)
[![License](https://img.shields.io/github/license/livekit/livekit)](https://github.com/livekit/livekit/blob/master/LICENSE)

<!--BEGIN_AGENTS_INFO-->
> [!IMPORTANT]
> If you're building Voice AI, [LiveKit Agents](https://github.com/livekit/agents) is the SDK for code-first realtime voice agents. STT, LLM, TTS, turn detection, [expressive speech](https://docs.livekit.io/agents/models/tts/expressive/), [keyterm accuracy](https://docs.livekit.io/agents/models/stt/keyterms/), tool usage, and telephony all come bundled in the framework. It's available in both [Python](https://github.com/livekit/agents) and [Node.js](https://github.com/livekit/agents-js).
>
> ```python
> # agent.py
> from livekit import agents
> from livekit.agents import Agent, AgentServer, AgentSession, STTContextOptions, TurnHandlingOptions, inference
>
> server = AgentServer()
>
>
> @server.rtc_session(agent_name="my-agent")
> async def my_agent(ctx: agents.JobContext):
>     session = AgentSession(
>         stt=inference.STT(model="deepgram/nova-3", language="multi"),
>         llm=inference.LLM(model="google/gemma-4-31b-it"),
>         tts=inference.TTS(model="inworld/inworld-tts-2", voice="Ashley"),
>         turn_handling=TurnHandlingOptions(turn_detection=inference.TurnDetector()),
>         stt_context_options=STTContextOptions(keyterms=["LiveKit", "Acme Corp"]),
>         expressive=True,
>     )
>     await session.start(room=ctx.room, agent=Agent(instructions="You are a helpful voice AI assistant."))
>     await session.generate_reply(instructions="Greet the user and offer your assistance.")
>
>
> if __name__ == "__main__":
>     agents.cli.run_app(server)
> ```
>
> Models come from [LiveKit Inference](https://docs.livekit.io/agents/models/) with no per-provider API keys, and LiveKit Cloud handles [deployment](https://docs.livekit.io/deploy/agents/) and [observability](https://docs.livekit.io/deploy/observability/). Visit the docs for more info at [docs.livekit.io/agents](https://docs.livekit.io/agents/).
<!--END_AGENTS_INFO-->

## Used in production by

LiveKit carries billions of calls a year for companies including Salesforce, Nvidia, Oracle, SAP,
Deutsche Telekom, Spotify, Tinder, Coursera, Headspace, Skydio, Retell, Decagon, Cresta, and HeyGen. Read how
[Assort Health](https://livekit.com/customers/assort-health), [Playback](https://livekit.com/customers/playback), and
[Polymath Robotics](https://livekit.com/customers/polymath) use it, or see [more customers](https://livekit.com/customers).

## Features

-   Scalable, distributed WebRTC SFU (Selective Forwarding Unit)
-   People, devices, and AI agents join the same room as participants, with
    [agent dispatch](https://docs.livekit.io/agents/server/agent-dispatch/) to route agents in automatically or on demand
-   Modern, full-featured SDKs for web, mobile, desktop, embedded, and server
-   Built for production, supports JWT authentication
-   Robust networking and connectivity, UDP/TCP/TURN
-   Easy to deploy: single binary, Docker or Kubernetes
-   Advanced features including:
    -   [speaker detection](https://docs.livekit.io/transport/media/subscribe/)
    -   [simulcast](https://docs.livekit.io/transport/media/publish/)
    -   [selective subscription](https://docs.livekit.io/transport/media/subscribe/)
    -   [moderation APIs](https://docs.livekit.io/intro/basics/rooms-participants-tracks/participants/)
    -   [end-to-end encryption](https://docs.livekit.io/transport/media/encryption/)
    -   SVC codecs (VP9, AV1)
    -   [data tracks](https://docs.livekit.io/transport/data/data-tracks/) for low-latency telemetry and teleoperation
    -   [telephony](https://docs.livekit.io/telephony/) over SIP
    -   [webhooks](https://docs.livekit.io/intro/basics/rooms-participants-tracks/webhooks-events/)
    -   [distributed and multi-region](https://docs.livekit.io/transport/self-hosting/distributed/)

## Documentation & Guides

https://docs.livekit.io

Working with a coding agent? Give it the [LiveKit Docs MCP server](https://docs.livekit.io/mcp/), or start with the
[coding agents guide](https://docs.livekit.io/intro/coding-agents/).

## Live Demos

-   [Talk to a voice agent](https://livekit.com) built with LiveKit Agents
-   [LiveKit Meet](https://meet.livekit.io) ([source](https://github.com/livekit-examples/meet))
-   [Spatial Audio](https://spatial-audio-demo.livekit.io/) ([source](https://github.com/livekit-examples/spatial-audio))
-   Livestreaming from OBS Studio ([source](https://github.com/livekit-examples/livestream))

## Install

> [!TIP]
> We recommend installing [LiveKit CLI](https://github.com/livekit/livekit-cli) along with the server. It lets you access
> server APIs, create tokens, generate test traffic, and scaffold and deploy agents.

The following will install LiveKit's media server:

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

To customize your setup for production, refer to our [deployment docs](https://docs.livekit.io/transport/self-hosting/deployment/)

### Creating access token

A user connecting to a LiveKit room requires an [access token](https://docs.livekit.io/frontends/build/authentication/). Access
tokens (JWT) encode the user's identity and the room permissions they've been granted. You can generate a token with our
CLI:

```shell
lk token create \
    --api-key devkey --api-secret secret \
    --join --room my-first-room --identity user1 \
    --valid-for 24h
```

### Test with example app

Head over to our [example app](https://example.livekit.io) and enter a generated token to connect to your LiveKit
server.

Once connected, your video and audio are now being published to your new LiveKit instance!

### Simulating a test publisher

```shell
lk room join \
    --url ws://localhost:7880 \
    --api-key devkey --api-secret secret \
    --identity bot-user1 \
    --publish-demo \
    my-first-room
```

This command publishes a looped demo video to a room. Due to how the video clip was encoded (keyframes every 3s),
there's a slight delay before the browser has sufficient data to begin rendering frames. This is an artifact of the
simulation.

### Adding an agent

Agents join rooms as participants, the same way a browser or a phone does. Follow the
[Voice AI quickstart](https://docs.livekit.io/agents/start/voice-ai/) to build one. An agent connects to a self-hosted
server the same way it connects to LiveKit Cloud; when running without Cloud, use
[model plugins](https://docs.livekit.io/agents/models/#plugins) in place of LiveKit Inference.

## Deployment

### Use LiveKit Cloud

LiveKit Cloud is the fastest and most reliable way to run LiveKit. It runs in 19+ regions with 99.99% uptime and adds
agent hosting, model inference, telephony, and observability on top of the server. The Build plan is free, with no
credit card required.

Sign up for [LiveKit Cloud](https://cloud.livekit.io/).

### Self-host

Read our [deployment docs](https://docs.livekit.io/transport/self-hosting/) for more information. Official
[Docker images](https://hub.docker.com/r/livekit/livekit-server) and [Helm charts](https://github.com/livekit/livekit-helm)
are available.

## Building from source

Pre-requisites:

-   Go 1.26+ is installed
-   GOPATH/bin is in your PATH

Then run

```shell
git clone https://github.com/livekit/livekit
cd livekit
./bootstrap.sh
mage
```

## Contributing

We welcome your contributions toward improving LiveKit! Please join us
[on Slack](http://livekit.io/join-slack) or in the [Developer Community](https://community.livekit.io) to discuss your
ideas and/or PRs.

## License

LiveKit server is licensed under Apache License v2.0.

<!--BEGIN_REPO_NAV-->
<br/><table>
<thead><tr><th colspan="2">LiveKit Ecosystem</th></tr></thead>
<tbody>
<tr><td>Agents SDKs</td><td><a href="https://github.com/livekit/agents">Python</a> · <a href="https://github.com/livekit/agents-js">Node.js</a></td></tr><tr></tr>
<tr><td>LiveKit SDKs</td><td><a href="https://github.com/livekit/client-sdk-js">Browser</a> · <a href="https://github.com/livekit/client-sdk-swift">Swift</a> · <a href="https://github.com/livekit/client-sdk-android">Android</a> · <a href="https://github.com/livekit/client-sdk-flutter">Flutter</a> · <a href="https://github.com/livekit/client-sdk-react-native">React Native</a> · <a href="https://github.com/livekit/rust-sdks">Rust</a> · <a href="https://github.com/livekit/node-sdks">Node.js</a> · <a href="https://github.com/livekit/python-sdks">Python</a> · <a href="https://github.com/livekit/client-sdk-unity">Unity</a> · <a href="https://github.com/livekit/client-sdk-unity-web">Unity (WebGL)</a> · <a href="https://github.com/livekit/client-sdk-esp32">ESP32</a> · <a href="https://github.com/livekit/client-sdk-cpp">C++</a></td></tr><tr></tr>
<tr><td>Starter Apps</td><td><a href="https://github.com/livekit-examples/agent-starter-python">Python Agent</a> · <a href="https://github.com/livekit-examples/agent-starter-node">TypeScript Agent</a> · <a href="https://github.com/livekit-examples/agent-starter-react">React App</a> · <a href="https://github.com/livekit-examples/agent-starter-swift">SwiftUI App</a> · <a href="https://github.com/livekit-examples/agent-starter-android">Android App</a> · <a href="https://github.com/livekit-examples/agent-starter-flutter">Flutter App</a> · <a href="https://github.com/livekit-examples/agent-starter-react-native">React Native App</a> · <a href="https://github.com/livekit-examples/agent-starter-embed">Web Embed</a></td></tr><tr></tr>
<tr><td>UI Components</td><td><a href="https://github.com/livekit/components-js">React</a> · <a href="https://github.com/livekit/components-android">Android Compose</a> · <a href="https://github.com/livekit/components-swift">SwiftUI</a> · <a href="https://github.com/livekit/components-flutter">Flutter</a></td></tr><tr></tr>
<tr><td>Server APIs</td><td><a href="https://github.com/livekit/node-sdks">Node.js</a> · <a href="https://github.com/livekit/server-sdk-go">Golang</a> · <a href="https://github.com/livekit/server-sdk-ruby">Ruby</a> · <a href="https://github.com/livekit/server-sdk-kotlin">Java/Kotlin</a> · <a href="https://github.com/livekit/python-sdks">Python</a> · <a href="https://github.com/livekit/rust-sdks">Rust</a> · <a href="https://github.com/agence104/livekit-server-sdk-php">PHP (community)</a> · <a href="https://github.com/pabloFuente/livekit-server-sdk-dotnet">.NET (community)</a></td></tr><tr></tr>
<tr><td>Resources</td><td><a href="https://docs.livekit.io">Docs</a> · <a href="https://docs.livekit.io/mcp">Docs MCP Server</a> · <a href="https://github.com/livekit/livekit-cli">CLI</a> · <a href="https://cloud.livekit.io">LiveKit Cloud</a></td></tr><tr></tr>
<tr><td>LiveKit Server OSS</td><td><b>LiveKit server</b> · <a href="https://github.com/livekit/egress">Egress</a> · <a href="https://github.com/livekit/ingress">Ingress</a> · <a href="https://github.com/livekit/sip">SIP</a></td></tr><tr></tr>
<tr><td>Community</td><td><a href="https://community.livekit.io">Developer Community</a> · <a href="https://livekit.io/join-slack">Slack</a> · <a href="https://x.com/livekit">X</a> · <a href="https://www.youtube.com/@livekit_io">YouTube</a></td></tr>
</tbody>
</table>
<!--END_REPO_NAV-->
