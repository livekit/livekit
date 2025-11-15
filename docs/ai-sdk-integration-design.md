# AI SDK Integration with LiveKit - Design Document

## Issue #4051 - LiveKit + AI SDK Integration

### Problem Statement
Currently, LiveKit agents must be written in Python using the LiveKit agents framework, which requires:
- Python knowledge and environment setup
- WebSocket connections to LiveKit server
- Working within Python's module system (lacks type safety)
- Limited flexibility for TypeScript/JavaScript developers

Many developers prefer TypeScript with frameworks like Vercel's AI SDK for:
- Type safety
- Better DX (Developer Experience)
- Existing TypeScript ecosystem
- Modern async/await patterns
- Easier integration with web applications

### Proposed Solution
Add **HTTP-based External Agent Support** to LiveKit server, allowing:
1. External agents (written in any language) to receive job notifications via HTTP webhooks
2. TypeScript/AI SDK agents to interact with LiveKit rooms through REST APIs
3. Flexible agent deployment (serverless, containers, traditional servers)

## Architecture

### Current Agent Flow (WebSocket-based)
```
LiveKit Server → WebSocket → Python Agent Worker
    ↓ (JobRequest)
    ← (AvailabilityResponse)
    → (JobAssignment + Token)
    ← (UpdateJobStatus)
```

### New External Agent Flow (HTTP-based)
```
LiveKit Server → HTTP POST → External Agent Service (AI SDK/TypeScript)
    ↓ (Job Notification with access token)
    
External Agent:
    1. Receives job notification
    2. Connects to room as participant (using provided token)
    3. Sends status updates via LiveKit REST API
    4. Performs AI/voice processing logic
```

## Implementation Components

### 1. Server-Side Changes (Go)

#### A. Agent Configuration Extension (`pkg/agent/config.go`)
```go
type Config struct {
    EnableUserDataRecording bool              `yaml:"enable_user_data_recording"`
    ExternalAgents          []ExternalAgent   `yaml:"external_agents"`
}

type ExternalAgent struct {
    Name            string            `yaml:"name"`
    WebhookURL      string            `yaml:"webhook_url"` // HTTP endpoint
    JobTypes        []livekit.JobType `yaml:"job_types"`   // JT_ROOM, JT_PUBLISHER, JT_PARTICIPANT
    Secret          string            `yaml:"secret"`      // For webhook signature validation
    Timeout         time.Duration     `yaml:"timeout"`
    RetryPolicy     RetryConfig       `yaml:"retry"`
    Headers         map[string]string `yaml:"headers"`
}

type RetryConfig struct {
    MaxRetries  int           `yaml:"max_retries"`
    InitialWait time.Duration `yaml:"initial_wait"`
    MaxWait     time.Duration `yaml:"max_wait"`
}
```

#### B. HTTP Agent Client (`pkg/agent/httpclient.go`)
New file to handle HTTP-based agent communication:
```go
type HTTPAgentClient struct {
    config     ExternalAgent
    httpClient *http.Client
}

func (c *HTTPAgentClient) SendJobNotification(ctx context.Context, job *livekit.Job, token string) error
func (c *HTTPAgentClient) GenerateWebhookSignature(payload []byte) string
```

#### C. External Agent Dispatcher (`pkg/agent/external_dispatcher.go`)
Dispatch jobs to external HTTP agents:
```go
type ExternalDispatcher struct {
    clients map[string]*HTTPAgentClient
}

func (d *ExternalDispatcher) DispatchJob(job *agent.JobRequest) error
```

#### D. Integration with Room Management (`pkg/rtc/room.go`)
Modify `launchRoomAgents` and `launchTargetAgents` to support external agents.

### 2. HTTP Webhook Payload

**POST to External Agent Webhook URL**
```json
{
  "job_id": "AJ_abc123",
  "dispatch_id": "AD_xyz789",
  "job_type": "JT_ROOM",
  "room": {
    "sid": "RM_abc",
    "name": "my-room",
    "created_at": 1234567890
  },
  "participant": {
    "sid": "PA_xyz",
    "identity": "user123"
  },
  "metadata": "custom-data",
  "access_token": "eyJhbGc...",  // Pre-generated access token
  "server_url": "wss://livekit.example.com",
  "timestamp": 1234567890
}
```

**Headers:**
- `X-LiveKit-Signature`: HMAC-SHA256 signature for validation
- `Content-Type`: `application/json`

**Expected Response (200 OK):**
```json
{
  "accepted": true,
  "participant_identity": "agent-bot-1",
  "participant_name": "AI Assistant",
  "participant_metadata": "{\"type\":\"ai-agent\"}"
}
```

### 3. External Agent Status Updates

External agents can update job status via REST API:
```
POST /api/agents/jobs/{job_id}/status
Authorization: Bearer <api_token>

{
  "status": "JS_RUNNING" | "JS_SUCCESS" | "JS_FAILED",
  "error": "optional error message"
}
```

### 4. Example TypeScript Integration

#### `ai-agent-server.ts`
```typescript
import { Vercel } from '@ai-sdk/openai';
import { LiveKit } from 'livekit-client';
import express from 'express';
import crypto from 'crypto';

const app = express();
const ai = new Vercel({ apiKey: process.env.OPENAI_API_KEY });

// Webhook handler for LiveKit job notifications
app.post('/livekit/agent', async (req, res) => {
  const { job_id, access_token, server_url, room } = req.body;
  
  // Validate webhook signature
  if (!validateSignature(req)) {
    return res.status(401).json({ error: 'Invalid signature' });
  }

  // Accept the job
  res.json({
    accepted: true,
    participant_identity: `ai-agent-${job_id}`,
    participant_name: 'AI Assistant'
  });

  // Connect to LiveKit room
  const livekitRoom = new LiveKit.Room();
  await livekitRoom.connect(server_url, access_token);

  // Handle audio with AI SDK
  livekitRoom.on('trackSubscribed', async (track, publication, participant) => {
    if (track.kind === 'audio') {
      const audioStream = track.mediaStream;
      
      // Process with AI SDK
      const result = await ai.generateText({
        model: 'gpt-4-turbo',
        prompt: 'Transcribe and respond to user audio',
        audioInput: audioStream
      });

      // Publish AI response as audio
      await publishAudioResponse(livekitRoom, result.text);
    }
  });

  // Update job status
  await updateJobStatus(job_id, 'JS_RUNNING');
});

function validateSignature(req: express.Request): boolean {
  const signature = req.headers['x-livekit-signature'];
  const payload = JSON.stringify(req.body);
  const hmac = crypto.createHmac('sha256', process.env.LIVEKIT_WEBHOOK_SECRET);
  const expected = hmac.update(payload).digest('hex');
  return signature === expected;
}

app.listen(3000, () => console.log('AI Agent server running'));
```

## Configuration Example

### `config.yaml`
```yaml
agents:
  enable_user_data_recording: true
  external_agents:
    - name: "ai-sdk-agent"
      webhook_url: "https://my-ai-service.com/livekit/agent"
      job_types:
        - JT_ROOM
        - JT_PUBLISHER
      secret: "your-webhook-secret"
      timeout: 10s
      retry:
        max_retries: 3
        initial_wait: 1s
        max_wait: 10s
      headers:
        X-Custom-Header: "value"
```

## Benefits

1. **Language Agnostic**: Write agents in TypeScript, Python, Go, Rust, etc.
2. **AI SDK Integration**: Easy integration with Vercel AI SDK, LangChain, etc.
3. **Flexible Deployment**: Deploy as serverless functions, containers, or traditional servers
4. **Type Safety**: Full TypeScript support for better DX
5. **Existing Infrastructure**: Use existing web frameworks and tools
6. **Easier Testing**: Standard HTTP testing tools
7. **Better Scaling**: Leverage cloud auto-scaling for HTTP services

## Migration Path

Existing Python agent users:
- No breaking changes
- WebSocket agents continue to work
- Can gradually migrate to HTTP-based agents

New users:
- Choose between WebSocket (Python SDK) or HTTP (any language)
- Clear documentation for both approaches

## Security Considerations

1. **Webhook Signature Validation**: HMAC-SHA256 signatures prevent unauthorized requests
2. **HTTPS Required**: Enforce HTTPS for external webhooks in production
3. **Token Expiration**: Access tokens have short TTL
4. **Rate Limiting**: Implement rate limits on webhook endpoints
5. **Secret Rotation**: Support secret rotation for webhook validation

## Next Steps

1. ✅ Create design document
2. Implement HTTP agent configuration
3. Create HTTP dispatcher
4. Add job lifecycle management
5. Build TypeScript example
6. Write tests
7. Documentation
8. Open PR for review

## Timeline

- Week 1: Core implementation (HTTP dispatcher, config)
- Week 2: TypeScript example + documentation
- Week 3: Testing + refinements
- Week 4: PR review + iteration

---

**Author**: Aditya Bhuran (@ADITYABHURAN)  
**Issue**: #4051  
**Branch**: feature/Aditya-Bhuran  
**Date**: November 9, 2025
