# LiveKit AI SDK Agent Example

This example demonstrates how to create a LiveKit agent using TypeScript and Vercel's AI SDK, enabling seamless integration with modern AI services.

## Features

- ✅ TypeScript with full type safety
- ✅ Vercel AI SDK integration
- ✅ Express.js webhook server
- ✅ Signature validation for secure webhooks
- ✅ Webhook pattern demonstration
- ⚠️ **Note**: This is a conceptual example showing the webhook integration pattern

## Important: Production Implementation

This example demonstrates the **webhook integration pattern**. For production audio processing:

1. **Use `livekit-server-sdk`** instead of `livekit-client` for Node.js
2. **Implement audio capture** using libraries like `@livekit/rtc-node` or `node-webrtc`
3. **Add transcription service** (Whisper, Deepgram, AssemblyAI)
4. **Implement TTS** (OpenAI TTS, ElevenLabs, etc.)

The core webhook handling shown here is production-ready. Audio processing requires additional implementation based on your specific needs.

## Prerequisites

- Node.js 18+ or 20+
- LiveKit Server with external agent support
- OpenAI API key (or other AI SDK compatible provider)

## Installation

```bash
npm install
```

## Configuration

Create a `.env` file:

```env
# LiveKit Configuration
LIVEKIT_URL=wss://your-livekit-server.com
LIVEKIT_API_KEY=your-api-key
LIVEKIT_API_SECRET=your-api-secret
LIVEKIT_WEBHOOK_SECRET=your-webhook-secret

# AI SDK Configuration
OPENAI_API_KEY=your-openai-api-key

# Server Configuration
PORT=3000
```

## LiveKit Server Configuration

Add this to your LiveKit server's `config.yaml`:

```yaml
agents:
  enable_user_data_recording: false
  external_agents:
    - name: "ai-sdk-agent"
      enabled: true
      webhook_url: "https://your-agent-server.com/livekit/agent"
      job_types:
        - "JT_ROOM"
        - "JT_PUBLISHER"
      secret: "your-webhook-secret"  # Must match LIVEKIT_WEBHOOK_SECRET
      timeout: 15s
      retry:
        max_retries: 3
        initial_wait: 1s
        max_wait: 10s
```

## Running the Agent

### Development Mode
```bash
npm run dev
```

### Production Mode
```bash
npm run build
npm start
```

## How It Works

### 1. Webhook Endpoint
The agent exposes a webhook endpoint at `/livekit/agent` that receives job notifications from LiveKit server.

### 2. Job Processing Flow

1. **Job Notification**: LiveKit server sends a POST request with job details
2. **Signature Validation**: Agent validates the HMAC signature
3. **Job Acceptance**: Agent responds with acceptance and participant details
4. **Room Connection**: Agent connects to the LiveKit room using the provided token
5. **Audio Processing**: Agent subscribes to participant audio tracks
6. **AI Processing**: Audio is transcribed and processed using AI SDK
7. **Response**: AI-generated responses are published back to the room

### 3. Webhook Payload

```json
{
  "job_id": "AJ_abc123",
  "dispatch_id": "AD_xyz789",
  "job_type": "JT_ROOM",
  "room": {
    "sid": "RM_abc",
    "name": "my-room"
  },
  "access_token": "eyJhbGc...",
  "server_url": "wss://livekit.example.com",
  "timestamp": 1234567890
}
```

### 4. Webhook Response

```json
{
  "accepted": true,
  "participant_identity": "ai-agent-bot",
  "participant_name": "AI Assistant",
  "participant_metadata": "{\"type\":\"ai-agent\"}"
}
```

## Project Structure

```
ai-sdk-agent/
├── src/
│   ├── index.ts          # Main server entry point
│   ├── agent.ts          # Agent logic and LiveKit room handling
│   ├── webhook.ts        # Webhook validation and handling
│   └── ai-processor.ts   # AI SDK integration
├── package.json
├── tsconfig.json
├── .env.example
└── README.md
```

## API Reference

### Webhook Endpoint: POST /livekit/agent

**Headers:**
- `Content-Type: application/json`
- `X-LiveKit-Signature: <hmac-sha256-signature>`
- `X-LiveKit-Timestamp: <unix-timestamp>`

**Request Body:** See Webhook Payload above

**Response (200 OK):**
```json
{
  "accepted": true,
  "participant_identity": "string",
  "participant_name": "string",
  "participant_metadata": "string"
}
```

**Response (401 Unauthorized):**
```json
{
  "error": "Invalid signature"
}
```

## Deployment

### Using Docker

```dockerfile
FROM node:20-alpine

WORKDIR /app

COPY package*.json ./
RUN npm ci --only=production

COPY . .
RUN npm run build

EXPOSE 3000

CMD ["npm", "start"]
```

### Using Vercel/Serverless

The agent can be deployed as a serverless function. See deployment guides for specific platforms.

### Environment Requirements

- Public HTTPS endpoint accessible by LiveKit server
- Webhook URL must be publicly accessible
- TLS/SSL certificate for production deployments

## Extending the Agent

### Custom AI Processing

Modify `src/ai-processor.ts` to add custom AI logic:

```typescript
export async function processAudio(audioBuffer: Buffer): Promise<string> {
  // Your custom AI processing
  const transcription = await transcribeAudio(audioBuffer);
  const response = await generateResponse(transcription);
  return response;
}
```

### Multiple Agent Types

You can run multiple agents with different configurations by creating separate webhook endpoints and registering them in LiveKit config.

## Troubleshooting

### Agent Not Receiving Jobs

1. Check webhook URL is publicly accessible
2. Verify webhook secret matches in both agent and LiveKit config
3. Check LiveKit server logs for webhook delivery errors

### Signature Validation Failures

1. Ensure LIVEKIT_WEBHOOK_SECRET matches server config
2. Verify system clocks are synchronized
3. Check for URL encoding issues

### Audio Processing Issues

1. Verify AI SDK API keys are valid
2. Check network connectivity to AI service
3. Monitor agent logs for processing errors

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

## License

Apache 2.0

## Support

- [LiveKit Documentation](https://docs.livekit.io)
- [AI SDK Documentation](https://sdk.vercel.ai/docs)
- [GitHub Issues](https://github.com/livekit/livekit/issues)
