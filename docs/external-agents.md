# AI SDK Integration with LiveKit

## Overview

LiveKit now supports **External HTTP-based Agents**, enabling developers to build agents in TypeScript, Python, Go, Rust, or any HTTP-capable language. This feature is particularly powerful for integrating with modern AI frameworks like **Vercel's AI SDK**, **LangChain**, and other AI services.

## Why External Agents?

### Traditional Approach (WebSocket-based Python Agents)
- Requires Python knowledge
- WebSocket connection management
- Limited to Python ecosystem
- Complex deployment

### New Approach (HTTP-based External Agents)
- ✅ **Language Agnostic**: Write in TypeScript, Python, Go, Rust, etc.
- ✅ **AI SDK Integration**: Easy integration with Vercel AI SDK, LangChain, etc.
- ✅ **Type Safety**: Full TypeScript support
- ✅ **Flexible Deployment**: Serverless, containers, traditional servers
- ✅ **Modern DX**: Use familiar web frameworks (Express, Fastify, etc.)
- ✅ **Easy Testing**: Standard HTTP testing tools
- ✅ **Better Scaling**: Leverage cloud auto-scaling

## How It Works

### Architecture Flow

```
┌──────────────────┐
│  LiveKit Server  │
└────────┬─────────┘
         │ HTTP POST (Job Notification)
         │ • job_id, room details
         │ • access_token for room
         │ • HMAC signature
         ▼
┌──────────────────┐
│ External Agent   │
│  (Your Service)  │
└────────┬─────────┘
         │ 1. Validate signature
         │ 2. Accept/reject job
         │ 3. Connect to room as participant
         │ 4. Process audio/video with AI
         │ 5. Respond in real-time
         └─────────────────────────►
```

## Configuration

### LiveKit Server Configuration

Add to your `config.yaml`:

```yaml
agents:
  enable_user_data_recording: false
  external_agents:
    - name: "ai-sdk-agent"
      enabled: true
      webhook_url: "https://your-agent-service.com/livekit/agent"
      job_types:
        - "JT_ROOM"          # Room-level agent
        - "JT_PUBLISHER"     # Participant publisher agent
      secret: "your-secure-webhook-secret"
      timeout: 15s
      retry:
        max_retries: 3
        initial_wait: 1s
        max_wait: 10s
      headers:
        X-Custom-Header: "value"
```

### Configuration Options

| Option | Type | Description |
|--------|------|-------------|
| `name` | string | Unique identifier for the external agent |
| `enabled` | boolean | Enable/disable this agent |
| `webhook_url` | string | HTTP endpoint to receive job notifications |
| `job_types` | array | Supported job types: `JT_ROOM`, `JT_PUBLISHER`, `JT_PARTICIPANT` |
| `secret` | string | Secret for HMAC signature validation (keep secure!) |
| `timeout` | duration | HTTP request timeout (default: 10s) |
| `retry` | object | Retry configuration for failed webhooks |
| `headers` | map | Custom HTTP headers to include in requests |

## Webhook Integration

### Job Notification Payload

LiveKit sends a POST request to your webhook URL:

```typescript
{
  "job_id": "AJ_abc123",           // Unique job identifier
  "dispatch_id": "AD_xyz789",      // Dispatch identifier
  "job_type": "JT_ROOM",           // Job type
  "room": {
    "sid": "RM_abc",
    "name": "my-room",
    "created_at": 1234567890
  },
  "participant": {                  // Only for publisher/participant jobs
    "sid": "PA_xyz",
    "identity": "user123"
  },
  "metadata": "custom-data",        // Optional metadata
  "access_token": "eyJhbGc...",     // Pre-generated room access token
  "server_url": "wss://livekit.example.com",
  "timestamp": 1234567890
}
```

### Webhook Headers

```
Content-Type: application/json
X-LiveKit-Signature: <hmac-sha256-signature>
X-LiveKit-Timestamp: <unix-timestamp>
```

### Webhook Response

Your agent should respond with:

```typescript
{
  "accepted": true,                          // Accept or reject the job
  "participant_identity": "ai-agent-bot",    // Identity for agent participant
  "participant_name": "AI Assistant",        // Display name
  "participant_metadata": "{\"type\":\"ai\"}" // Optional metadata
}
```

### Signature Validation

Always validate the HMAC signature:

```typescript
import crypto from 'crypto';

function validateSignature(payload: string, signature: string, secret: string): boolean {
  const hmac = crypto.createHmac('sha256', secret);
  const expected = hmac.update(payload).digest('hex');
  return crypto.timingSafeEqual(
    Buffer.from(signature),
    Buffer.from(expected)
  );
}
```

## Quick Start Examples

### TypeScript with AI SDK

See the complete example in `examples/ai-sdk-agent/`:

```typescript
import express from 'express';
import { Room } from 'livekit-client';
import { generateText, openai } from 'ai';

const app = express();

app.post('/livekit/agent', async (req, res) => {
  // 1. Validate signature
  if (!validateSignature(req)) {
    return res.status(401).json({ accepted: false, error: 'Invalid signature' });
  }

  const { job_id, access_token, server_url, room } = req.body;

  // 2. Accept job
  res.json({
    accepted: true,
    participant_identity: `ai-agent-${job_id}`,
    participant_name: 'AI Assistant'
  });

  // 3. Connect to room
  const livekitRoom = new Room();
  await livekitRoom.connect(server_url, access_token);

  // 4. Process audio with AI SDK
  livekitRoom.on('trackSubscribed', async (track) => {
    if (track.kind === 'audio') {
      const transcription = await transcribeAudio(track);
      const response = await generateText({
        model: openai('gpt-4-turbo'),
        prompt: `Respond to: ${transcription}`
      });
      await publishResponse(livekitRoom, response.text);
    }
  });
});

app.listen(3000);
```

### Python with FastAPI

```python
from fastapi import FastAPI, Request, HTTPException
from livekit import rtc
import hmac
import hashlib

app = FastAPI()

@app.post("/livekit/agent")
async def handle_webhook(request: Request):
    # 1. Validate signature
    payload = await request.body()
    signature = request.headers.get('x-livekit-signature')
    if not validate_signature(payload, signature):
        raise HTTPException(status_code=401, detail="Invalid signature")
    
    data = await request.json()
    
    # 2. Accept job
    response = {
        "accepted": True,
        "participant_identity": f"ai-agent-{data['job_id']}",
        "participant_name": "AI Assistant"
    }
    
    # 3. Connect to room in background
    asyncio.create_task(connect_agent(data))
    
    return response

def validate_signature(payload: bytes, signature: str) -> bool:
    expected = hmac.new(
        SECRET.encode(),
        payload,
        hashlib.sha256
    ).hexdigest()
    return hmac.compare_digest(signature, expected)
```

### Go with Standard Library

```go
package main

import (
    "crypto/hmac"
    "crypto/sha256"
    "encoding/hex"
    "encoding/json"
    "net/http"
)

type JobNotification struct {
    JobID       string `json:"job_id"`
    AccessToken string `json:"access_token"`
    ServerURL   string `json:"server_url"`
    Room        struct {
        Name string `json:"name"`
    } `json:"room"`
}

type WebhookResponse struct {
    Accepted            bool   `json:"accepted"`
    ParticipantIdentity string `json:"participant_identity"`
    ParticipantName     string `json:"participant_name"`
}

func handleWebhook(w http.ResponseWriter, r *http.Request) {
    // 1. Validate signature
    if !validateSignature(r) {
        http.Error(w, "Invalid signature", http.StatusUnauthorized)
        return
    }
    
    var notification JobNotification
    json.NewDecoder(r.Body).Decode(&notification)
    
    // 2. Accept job
    response := WebhookResponse{
        Accepted:            true,
        ParticipantIdentity: "ai-agent-" + notification.JobID,
        ParticipantName:     "AI Assistant",
    }
    
    json.NewEncoder(w).Encode(response)
    
    // 3. Connect to room in goroutine
    go connectAgent(notification)
}

func validateSignature(r *http.Request) bool {
    // Implementation
    return true
}
```

## Deployment Guide

### Local Development

```bash
# Start LiveKit server
livekit-server --dev --config config.yaml

# Start your agent
cd examples/ai-sdk-agent
npm install
npm run dev
```

### Production Deployment

#### Docker

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

#### Kubernetes

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: livekit-ai-agent
spec:
  replicas: 3
  template:
    spec:
      containers:
      - name: agent
        image: your-agent:latest
        env:
        - name: LIVEKIT_WEBHOOK_SECRET
          valueFrom:
            secretKeyRef:
              name: livekit-secrets
              key: webhook-secret
```

#### Serverless (Vercel/AWS Lambda)

External agents work great with serverless platforms:

- **Vercel**: Deploy as API routes
- **AWS Lambda**: Use API Gateway + Lambda
- **Cloud Functions**: Google Cloud Functions
- **Azure Functions**: HTTP-triggered functions

## Security Best Practices

1. **Always Validate Signatures**: Never accept webhooks without HMAC validation
2. **Use HTTPS**: Enforce TLS for all webhook endpoints in production
3. **Rotate Secrets**: Periodically rotate webhook secrets
4. **Rate Limiting**: Implement rate limiting on webhook endpoints
5. **Timeout Management**: Set appropriate timeouts for long-running AI operations
6. **Error Handling**: Gracefully handle AI service failures

## Monitoring & Debugging

### Logging

```typescript
console.log('Webhook received:', {
  jobId: notification.job_id,
  room: notification.room.name,
  timestamp: new Date().toISOString()
});
```

### Metrics

Track important metrics:
- Webhook success/failure rate
- Job acceptance rate
- AI processing latency
- Room connection success rate

### Health Checks

Implement health check endpoints:

```typescript
app.get('/health', (req, res) => {
  res.json({
    status: 'healthy',
    uptime: process.uptime(),
    activeJobs: agentManager.getActiveCount()
  });
});
```

## Troubleshooting

### Agent Not Receiving Jobs

- ✓ Verify webhook URL is publicly accessible
- ✓ Check LiveKit server logs for webhook delivery errors
- ✓ Ensure `enabled: true` in config
- ✓ Verify job types match

### Signature Validation Failures

- ✓ Secret must match exactly in both configs
- ✓ Check for URL encoding issues
- ✓ Verify system clocks are synchronized

### Connection Issues

- ✓ Access token must not be expired
- ✓ Server URL must be WebSocket-compatible
- ✓ Check network connectivity

## Migration from Python Agents

External agents work alongside traditional WebSocket agents:

1. **No Breaking Changes**: Existing Python agents continue to work
2. **Gradual Migration**: Migrate agents one at a time
3. **Feature Parity**: External agents support all agent features
4. **Performance**: Similar performance characteristics

## Contributing

This feature is brand new! We welcome contributions:

- 📝 Documentation improvements
- 🐛 Bug reports
- 💡 Feature requests
- 🔧 Pull requests

See [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines.

## Resources

- [LiveKit Documentation](https://docs.livekit.io)
- [AI SDK Documentation](https://sdk.vercel.ai/docs)
- [Example Projects](./examples/ai-sdk-agent/)
- [GitHub Discussions](https://github.com/livekit/livekit/discussions)

## Support

- **GitHub Issues**: [livekit/livekit/issues](https://github.com/livekit/livekit/issues)
- **Slack Community**: [livekit.io/join-slack](https://livekit.io/join-slack)
- **Twitter**: [@livekit](https://twitter.com/livekit)

---

**Feature Author**: @ADITYABHURAN  
**Issue**: #4051  
**Status**: ✅ Ready for Review
