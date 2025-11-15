// SPDX-FileCopyrightText: 2024 LiveKit, Inc.
//
// SPDX-License-Identifier: Apache-2.0
import type { RoomConfiguration } from '@livekit/protocol';
import { TrackSource } from '@livekit/protocol';
import type { JWTPayload } from 'jose';

export function trackSourceToString(source: TrackSource) {
  switch (source) {
    case TrackSource.CAMERA:
      return 'camera';
    case TrackSource.MICROPHONE:
      return 'microphone';
    case TrackSource.SCREEN_SHARE:
      return 'screen_share';
    case TrackSource.SCREEN_SHARE_AUDIO:
      return 'screen_share_audio';
    default:
      throw new TypeError(`Cannot convert TrackSource ${source} to string`);
  }
}

export function claimsToJwtPayload(
  grant: ClaimGrants,
): JWTPayload & { video?: Record<string, unknown> } {
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const claim: Record<string, any> = { ...grant };
  // eslint-disable-next-line no-restricted-syntax
  if (Array.isArray(claim.video?.canPublishSources)) {
    claim.video.canPublishSources = claim.video.canPublishSources.map(trackSourceToString);
  }
  return claim;
}

export interface VideoGrant {
  /** permission to create a room */
  roomCreate?: boolean;

  /** permission to join a room as a participant, room must be set */
  roomJoin?: boolean;

  /** permission to list rooms */
  roomList?: boolean;

  /** permission to start a recording */
  roomRecord?: boolean;

  /** permission to control a specific room, room must be set */
  roomAdmin?: boolean;

  /** name of the room, must be set for admin or join permissions */
  room?: string;

  /** permissions to control ingress, not specific to any room or ingress */
  ingressAdmin?: boolean;

  /**
   * allow participant to publish. If neither canPublish or canSubscribe is set,
   * both publish and subscribe are enabled
   */
  canPublish?: boolean;

  /**
   * TrackSource types that the participant is allowed to publish
   * When set, it supersedes CanPublish. Only sources explicitly set here can be published
   */
  canPublishSources?: TrackSource[];

  /** allow participant to subscribe to other tracks */
  canSubscribe?: boolean;

  /**
   * allow participants to publish data, defaults to true if not set
   */
  canPublishData?: boolean;

  /**
   * by default, a participant is not allowed to update its own metadata
   */
  canUpdateOwnMetadata?: boolean;

  /** participant isn't visible to others */
  hidden?: boolean;

  /** participant is recording the room, when set, allows room to indicate it's being recorded */
  recorder?: boolean;

  /** participant allowed to connect to LiveKit as Agent Framework worker */
  agent?: boolean;

  /** allow participant to subscribe to metrics */
  canSubscribeMetrics?: boolean;

  /** destination room which this participant can forward to */
  destinationRoom?: string;
}

export interface SIPGrant {
  /** manage sip resources */
  admin?: boolean;

  /** make outbound calls */
  call?: boolean;
}

export interface InferenceGrant {
  /** perform inference */
  perform?: boolean;
}

/** @internal */
export interface ClaimGrants extends JWTPayload {
  name?: string;
  video?: VideoGrant;
  sip?: SIPGrant;
  inference?: InferenceGrant;
  kind?: string;
  metadata?: string;
  attributes?: Record<string, string>;
  sha256?: string;
  roomPreset?: string;
  roomConfig?: RoomConfiguration;
}
