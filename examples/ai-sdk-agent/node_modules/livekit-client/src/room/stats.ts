export const monitorFrequency = 2000;

// key stats for senders and receivers
interface SenderStats {
  /** number of packets sent */
  packetsSent?: number;

  /** number of bytes sent */
  bytesSent?: number;

  /** jitter as perceived by remote */
  jitter?: number;

  /** packets reported lost by remote */
  packetsLost?: number;

  /** RTT reported by remote */
  roundTripTime?: number;

  /** ID of the outbound stream */
  streamId?: string;

  timestamp: number;
}

export interface AudioSenderStats extends SenderStats {
  type: 'audio';
}

export interface VideoSenderStats extends SenderStats {
  type: 'video';

  firCount: number;

  pliCount: number;

  nackCount: number;

  rid: string;

  frameWidth: number;

  frameHeight: number;

  framesPerSecond: number;

  framesSent: number;

  // bandwidth, cpu, other, none
  qualityLimitationReason?: string;

  qualityLimitationDurations?: Record<string, number>;

  qualityLimitationResolutionChanges?: number;

  retransmittedPacketsSent?: number;

  targetBitrate: number;
}

interface ReceiverStats {
  jitterBufferDelay?: number;

  /** packets reported lost by remote */
  packetsLost?: number;

  /** number of packets sent */
  packetsReceived?: number;

  bytesReceived?: number;

  streamId?: string;

  jitter?: number;

  timestamp: number;
}

export interface AudioReceiverStats extends ReceiverStats {
  type: 'audio';

  concealedSamples?: number;

  concealmentEvents?: number;

  silentConcealedSamples?: number;

  silentConcealmentEvents?: number;

  totalAudioEnergy?: number;

  totalSamplesDuration?: number;
}

export interface VideoReceiverStats extends ReceiverStats {
  type: 'video';

  framesDecoded: number;

  framesDropped: number;

  framesReceived: number;

  frameWidth?: number;

  frameHeight?: number;

  firCount?: number;

  pliCount?: number;

  nackCount?: number;

  decoderImplementation?: string;

  mimeType?: string;
}

export function computeBitrate<T extends ReceiverStats | SenderStats>(
  currentStats: T,
  prevStats?: T,
): number {
  if (!prevStats) {
    return 0;
  }
  let bytesNow: number | undefined;
  let bytesPrev: number | undefined;
  if ('bytesReceived' in currentStats) {
    bytesNow = (currentStats as ReceiverStats).bytesReceived;
    bytesPrev = (prevStats as ReceiverStats).bytesReceived;
  } else if ('bytesSent' in currentStats) {
    bytesNow = (currentStats as SenderStats).bytesSent;
    bytesPrev = (prevStats as SenderStats).bytesSent;
  }
  if (
    bytesNow === undefined ||
    bytesPrev === undefined ||
    currentStats.timestamp === undefined ||
    prevStats.timestamp === undefined
  ) {
    return 0;
  }
  return ((bytesNow - bytesPrev) * 8 * 1000) / (currentStats.timestamp - prevStats.timestamp);
}
