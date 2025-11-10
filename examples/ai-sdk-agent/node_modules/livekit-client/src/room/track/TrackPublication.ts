import { Encryption_Type } from '@livekit/protocol';
import type {
  SubscriptionError,
  TrackInfo,
  UpdateSubscription,
  UpdateTrackSettings,
} from '@livekit/protocol';
import { EventEmitter } from 'events';
import type TypedEventEmitter from 'typed-emitter';
import log, { LoggerNames, getLogger } from '../../logger';
import { TrackEvent } from '../events';
import type { LoggerOptions, TranscriptionSegment } from '../types';
import { isAudioTrack, isVideoTrack } from '../utils';
import LocalAudioTrack from './LocalAudioTrack';
import LocalVideoTrack from './LocalVideoTrack';
import RemoteAudioTrack from './RemoteAudioTrack';
import type RemoteTrack from './RemoteTrack';
import RemoteVideoTrack from './RemoteVideoTrack';
import { Track } from './Track';
import { getLogContextFromTrack } from './utils';

export abstract class TrackPublication extends (EventEmitter as new () => TypedEventEmitter<PublicationEventCallbacks>) {
  kind: Track.Kind;

  trackName: string;

  trackSid: Track.SID;

  track?: Track;

  source: Track.Source;

  /** MimeType of the published track */
  mimeType?: string;

  /** dimension of the original published stream, video-only */
  dimensions?: Track.Dimensions;

  /** true if track was simulcasted to server, video-only */
  simulcasted?: boolean;

  /** @internal */
  trackInfo?: TrackInfo;

  protected metadataMuted: boolean = false;

  protected encryption: Encryption_Type = Encryption_Type.NONE;

  protected log = log;

  private loggerContextCb?: LoggerOptions['loggerContextCb'];

  constructor(kind: Track.Kind, id: string, name: string, loggerOptions?: LoggerOptions) {
    super();
    this.log = getLogger(loggerOptions?.loggerName ?? LoggerNames.Publication);
    this.loggerContextCb = this.loggerContextCb;
    this.setMaxListeners(100);
    this.kind = kind;
    this.trackSid = id;
    this.trackName = name;
    this.source = Track.Source.Unknown;
  }

  /** @internal */
  setTrack(track?: Track) {
    if (this.track) {
      this.track.off(TrackEvent.Muted, this.handleMuted);
      this.track.off(TrackEvent.Unmuted, this.handleUnmuted);
    }

    this.track = track;

    if (track) {
      // forward events
      track.on(TrackEvent.Muted, this.handleMuted);
      track.on(TrackEvent.Unmuted, this.handleUnmuted);
    }
  }

  protected get logContext() {
    return {
      ...this.loggerContextCb?.(),
      ...getLogContextFromTrack(this),
    };
  }

  get isMuted(): boolean {
    return this.metadataMuted;
  }

  get isEnabled(): boolean {
    return true;
  }

  get isSubscribed(): boolean {
    return this.track !== undefined;
  }

  get isEncrypted(): boolean {
    return this.encryption !== Encryption_Type.NONE;
  }

  abstract get isLocal(): boolean;

  /**
   * an [AudioTrack] if this publication holds an audio track
   */
  get audioTrack(): LocalAudioTrack | RemoteAudioTrack | undefined {
    if (isAudioTrack(this.track)) {
      return this.track;
    }
  }

  /**
   * an [VideoTrack] if this publication holds a video track
   */
  get videoTrack(): LocalVideoTrack | RemoteVideoTrack | undefined {
    if (isVideoTrack(this.track)) {
      return this.track;
    }
  }

  handleMuted = () => {
    this.emit(TrackEvent.Muted);
  };

  handleUnmuted = () => {
    this.emit(TrackEvent.Unmuted);
  };

  /** @internal */
  updateInfo(info: TrackInfo) {
    this.trackSid = info.sid;
    this.trackName = info.name;
    this.source = Track.sourceFromProto(info.source);
    this.mimeType = info.mimeType;
    if (this.kind === Track.Kind.Video && info.width > 0) {
      this.dimensions = {
        width: info.width,
        height: info.height,
      };
      this.simulcasted = info.simulcast;
    }
    this.encryption = info.encryption;
    this.trackInfo = info;
    this.log.debug('update publication info', { ...this.logContext, info });
  }
}

export namespace TrackPublication {
  export enum SubscriptionStatus {
    Desired = 'desired',
    Subscribed = 'subscribed',
    Unsubscribed = 'unsubscribed',
  }

  export enum PermissionStatus {
    Allowed = 'allowed',
    NotAllowed = 'not_allowed',
  }
}

export type PublicationEventCallbacks = {
  muted: () => void;
  unmuted: () => void;
  ended: (track?: Track) => void;
  updateSettings: (settings: UpdateTrackSettings) => void;
  subscriptionPermissionChanged: (
    status: TrackPublication.PermissionStatus,
    prevStatus: TrackPublication.PermissionStatus,
  ) => void;
  updateSubscription: (sub: UpdateSubscription) => void;
  subscribed: (track: RemoteTrack) => void;
  unsubscribed: (track: RemoteTrack) => void;
  subscriptionStatusChanged: (
    status: TrackPublication.SubscriptionStatus,
    prevStatus: TrackPublication.SubscriptionStatus,
  ) => void;
  subscriptionFailed: (error: SubscriptionError) => void;
  transcriptionReceived: (transcription: TranscriptionSegment[]) => void;
  timeSyncUpdate: (timestamp: number) => void;
  cpuConstrained: (track: LocalVideoTrack) => void;
};
