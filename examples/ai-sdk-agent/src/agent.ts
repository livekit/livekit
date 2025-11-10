/**
 * Agent Manager - Handles LiveKit room connections and AI interactions
 */

import { Room, RoomEvent, RemoteTrack, RemoteParticipant, Track } from 'livekit-client';
import { processWithAI } from './ai-processor';

export interface AgentConfig {
  jobId: string;
  roomName: string;
  serverUrl: string;
  accessToken: string;
  participantIdentity: string;
  metadata?: string;
}

export class AgentManager {
  private activeAgents: Map<string, Room> = new Map();

  /**
   * Start an agent for a specific job
   */
  async startAgent(config: AgentConfig): Promise<void> {
    const { jobId, roomName, serverUrl, accessToken, participantIdentity } = config;

    console.log(`🚀 Starting agent for job: ${jobId}`);
    console.log(`   Room: ${roomName}`);
    console.log(`   Identity: ${participantIdentity}`);

    try {
      // Create LiveKit room instance
      const room = new Room({
        adaptiveStream: true,
        dynacast: true,
      });

      // Store active agent
      this.activeAgents.set(jobId, room);

      // Set up event handlers
      this.setupRoomHandlers(room, jobId);

      // Connect to the room
      await room.connect(serverUrl, accessToken);

      console.log(`✅ Agent connected to room: ${roomName}`);

      // Subscribe to all participant tracks
      this.subscribeToTracks(room);

    } catch (error) {
      console.error(`❌ Failed to start agent for job ${jobId}:`, error);
      this.activeAgents.delete(jobId);
      throw error;
    }
  }

  /**
   * Set up room event handlers
   */
  private setupRoomHandlers(room: Room, jobId: string): void {
    room.on(RoomEvent.TrackSubscribed, (
      track: RemoteTrack,
      publication: any,
      participant: RemoteParticipant
    ) => {
      console.log(`🎤 Subscribed to track from ${participant.identity}`);

      if (track.kind === Track.Kind.Audio) {
        this.handleAudioTrack(track, participant, jobId);
      }
    });

    room.on(RoomEvent.TrackUnsubscribed, (
      track: RemoteTrack,
      publication: any,
      participant: RemoteParticipant
    ) => {
      console.log(`🔇 Unsubscribed from track from ${participant.identity}`);
    });

    room.on(RoomEvent.ParticipantConnected, (participant: RemoteParticipant) => {
      console.log(`👋 Participant joined: ${participant.identity}`);
    });

    room.on(RoomEvent.ParticipantDisconnected, (participant: RemoteParticipant) => {
      console.log(`👋 Participant left: ${participant.identity}`);
    });

    room.on(RoomEvent.Disconnected, () => {
      console.log(`🔌 Disconnected from room (Job: ${jobId})`);
      this.activeAgents.delete(jobId);
    });

    room.on(RoomEvent.Reconnecting, () => {
      console.log(`🔄 Reconnecting... (Job: ${jobId})`);
    });

    room.on(RoomEvent.Reconnected, () => {
      console.log(`✅ Reconnected (Job: ${jobId})`);
    });
  }

  /**
   * Subscribe to all available tracks in the room
   */
  private subscribeToTracks(room: Room): void {
    room.remoteParticipants.forEach(participant => {
      participant.trackPublications.forEach(publication => {
        if (!publication.isSubscribed) {
          publication.setSubscribed(true);
        }
      });
    });
  }

  /**
   * Handle incoming audio track from participant
   */
  private async handleAudioTrack(
    track: RemoteTrack,
    participant: RemoteParticipant,
    jobId: string
  ): Promise<void> {
    console.log(`🎧 Processing audio from ${participant.identity}`);

    try {
      // Get the MediaStreamTrack
      const mediaStreamTrack = track.mediaStreamTrack;
      if (!mediaStreamTrack) {
        console.error('No media stream track available');
        return;
      }

      // Create MediaStream
      const mediaStream = new MediaStream([mediaStreamTrack]);

      // Process audio with AI
      // Note: This is a simplified example. In production, you'd need to:
      // 1. Capture audio chunks
      // 2. Send to transcription service
      // 3. Process with AI
      // 4. Generate response audio
      // 5. Publish back to room

      const audioContext = {
        participant: participant.identity,
        jobId,
        stream: mediaStream
      };

      // Process with AI SDK
      await processWithAI(audioContext);

    } catch (error) {
      console.error('Error processing audio track:', error);
    }
  }

  /**
   * Stop an agent by job ID
   */
  async stopAgent(jobId: string): Promise<void> {
    const room = this.activeAgents.get(jobId);
    if (room) {
      console.log(`🛑 Stopping agent for job: ${jobId}`);
      await room.disconnect();
      this.activeAgents.delete(jobId);
    }
  }

  /**
   * Get active agent count
   */
  getActiveCount(): number {
    return this.activeAgents.size;
  }

  /**
   * Stop all active agents
   */
  async stopAll(): Promise<void> {
    console.log(`🛑 Stopping all ${this.activeAgents.size} active agents`);
    const promises = Array.from(this.activeAgents.keys()).map(jobId =>
      this.stopAgent(jobId)
    );
    await Promise.all(promises);
  }
}
