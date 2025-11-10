/**
 * LiveKit AI SDK Agent Example
 * Main entry point for the agent server
 */

import express from 'express';
import dotenv from 'dotenv';
import { handleWebhook } from './webhook';

// Load environment variables
dotenv.config();

const app = express();
const PORT = process.env.PORT || 3000;

// Middleware
app.use(express.json());
app.use(express.urlencoded({ extended: true }));

// Health check endpoint
app.get('/health', (req, res) => {
  res.json({
    status: 'healthy',
    timestamp: new Date().toISOString(),
    version: '1.0.0'
  });
});

// LiveKit agent webhook endpoint
app.post('/livekit/agent', handleWebhook);

// Error handling middleware
app.use((err: Error, req: express.Request, res: express.Response, next: express.NextFunction) => {
  console.error('Error:', err);
  res.status(500).json({
    error: 'Internal server error',
    message: err.message
  });
});

// Start server
app.listen(PORT, () => {
  console.log(`🤖 LiveKit AI SDK Agent running on port ${PORT}`);
  console.log(`📡 Webhook endpoint: http://localhost:${PORT}/livekit/agent`);
  console.log(`💚 Health check: http://localhost:${PORT}/health`);
});

export default app;
