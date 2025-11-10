/**
 * Webhook handler for LiveKit agent job notifications
 */

import { Request, Response } from 'express';
import crypto from 'crypto';
import { AgentManager } from './agent';

// Types for LiveKit webhook payload
export interface LiveKitJobNotification {
  job_id: string;
  dispatch_id: string;
  job_type: 'JT_ROOM' | 'JT_PUBLISHER' | 'JT_PARTICIPANT';
  room: {
    sid: string;
    name: string;
    created_at?: number;
  };
  participant?: {
    sid: string;
    identity: string;
  };
  metadata?: string;
  access_token: string;
  server_url: string;
  timestamp: number;
}

export interface WebhookResponse {
  accepted: boolean;
  participant_identity?: string;
  participant_name?: string;
  participant_metadata?: string;
  participant_attributes?: Record<string, string>;
  error?: string;
}

const agentManager = new AgentManager();

/**
 * Validates HMAC signature from LiveKit server
 */
function validateSignature(req: Request): boolean {
  const signature = req.headers['x-livekit-signature'] as string;
  const webhookSecret = process.env.LIVEKIT_WEBHOOK_SECRET;

  if (!signature || !webhookSecret) {
    console.error('Missing signature or webhook secret');
    return false;
  }

  const payload = JSON.stringify(req.body);
  const hmac = crypto.createHmac('sha256', webhookSecret);
  const expectedSignature = hmac.update(payload).digest('hex');

  return crypto.timingSafeEqual(
    Buffer.from(signature),
    Buffer.from(expectedSignature)
  );
}

/**
 * Main webhook handler for LiveKit agent job notifications
 */
export async function handleWebhook(req: Request, res: Response): Promise<void> {
  try {
    console.log('📨 Received webhook request');

    // Validate signature
    if (!validateSignature(req)) {
      console.error('❌ Invalid webhook signature');
      res.status(401).json({
        accepted: false,
        error: 'Invalid signature'
      });
      return;
    }

    const notification: LiveKitJobNotification = req.body;

    console.log('✅ Webhook signature validated');
    console.log(`📋 Job ID: ${notification.job_id}`);
    console.log(`🏠 Room: ${notification.room.name}`);
    console.log(`📊 Job Type: ${notification.job_type}`);

    // Generate participant details
    const participantIdentity = `ai-agent-${notification.job_id.slice(-8)}`;
    const participantName = 'AI Assistant';
    const participantMetadata = JSON.stringify({
      type: 'ai-agent',
      job_id: notification.job_id,
      created_at: Date.now()
    });

    // Accept the job
    const response: WebhookResponse = {
      accepted: true,
      participant_identity: participantIdentity,
      participant_name: participantName,
      participant_metadata: participantMetadata
    };

    res.status(200).json(response);

    console.log(`✅ Job accepted, agent will join as: ${participantIdentity}`);

    // Start agent in background
    agentManager.startAgent({
      jobId: notification.job_id,
      roomName: notification.room.name,
      serverUrl: notification.server_url,
      accessToken: notification.access_token,
      participantIdentity,
      metadata: notification.metadata
    }).catch(error => {
      console.error(`❌ Error starting agent for job ${notification.job_id}:`, error);
    });

  } catch (error) {
    console.error('❌ Webhook error:', error);
    res.status(500).json({
      accepted: false,
      error: 'Internal server error'
    });
  }
}
