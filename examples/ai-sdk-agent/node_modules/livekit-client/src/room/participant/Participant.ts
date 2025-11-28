import {
  DataPacket_Kind,
  Encryption_Type,
  ParticipantInfo,
  ParticipantInfo_State,
  ParticipantInfo_Kind as ParticipantKind,
  ParticipantPermission,
  ConnectionQuality as ProtoQuality,
  type SipDTMF,
  SubscriptionError,
} from '@livekit/protocol';
import { EventEmitter } from 'events';
import type TypedEmitter from 'typed-emitter';
import log, { LoggerNames, type StructuredLogger, getLogger } from '../../logger';
import { ParticipantEvent, TrackEvent } from '../events';
import type LocalTrackPublication from '../track/LocalTrackPublication';
import type LocalVideoTrack from '../track/LocalVideoTrack';
import type RemoteTrack from '../track/RemoteTrack';
import type RemoteTrackPublication from '../track/RemoteTrackPublication';
import { Track } from '../track/Track';
import type { TrackPublication } from '../track/TrackPublication';
import { diffAttributes } from '../track/utils';
import type { ChatMessage, LoggerOptions, TranscriptionSegment } from '../types';
import { Future, isAudioTrack } from '../utils';

export enum ConnectionQuality {
  Excellent = 'excellent',
  Good = 'good',
  Poor = 'poor',
  /**
   * Indicates that a participant has temporarily (or permanently) lost connection to LiveKit.
   * For permanent disconnection a `ParticipantDisconnected` event will be emitted after a timeout
   */
  Lost = 'lost',
  Unknown = 'unknown',
}

function qualityFromProto(q: ProtoQuality): ConnectionQuality {
  switch (q) {
    case ProtoQuality.EXCELLENT:
      return ConnectionQuality.Excellent;
    case ProtoQuality.GOOD:
      return ConnectionQuality.Good;
    case ProtoQuality.POOR:
      return ConnectionQuality.Poor;
    case ProtoQuality.LOST:
      return ConnectionQuality.Lost;
    default:
      return ConnectionQuality.Unknown;
  }
}

export { ParticipantKind };

export default class Participant extends (EventEmitter as new () => TypedEmitter<ParticipantEventCallbacks>) {
  protected participantInfo?: ParticipantInfo;

  audioTrackPublications: Map<string, TrackPublication>;

  videoTrackPublications: Map<string, TrackPublication>;

  /** map of track sid => all published tracks */
  trackPublications: Map<string, TrackPublication>;

  /** audio level between 0-1.0, 1 being loudest, 0 being softest */
  audioLevel: number = 0;

  /** if participant is currently speaking */
  isSpeaking: boolean = false;

  /** server assigned unique id */
  sid: string;

  /** client assigned identity, encoded in JWT token */
  identity: string;

  /** client assigned display name, encoded in JWT token */
  name?: string;

  /** client metadata, opaque to livekit */
  metadata?: string;

  private _attributes: Record<string, string>;

  lastSpokeAt?: Date | undefined;

  permissions?: ParticipantPermission;

  protected _kind: ParticipantKind;

  private _connectionQuality: ConnectionQuality = ConnectionQuality.Unknown;

  protected audioContext?: AudioContext;

  protected log: StructuredLogger = log;

  protected loggerOptions?: LoggerOptions;

  protected activeFuture?: Future<void>;

  protected get logContext() {
    return {
      ...this.loggerOptions?.loggerContextCb?.(),
    };
  }

  get isEncrypted() {
    return (
      this.trackPublications.size > 0 &&
      Array.from(this.trackPublications.values()).every((tr) => tr.isEncrypted)
    );
  }

  get isAgent() {
    return this.permissions?.agent || this.kind === ParticipantKind.AGENT;
  }

  get isActive() {
    return this.participantInfo?.state === ParticipantInfo_State.ACTIVE;
  }

  get kind() {
    return this._kind;
  }

  /** participant attributes, similar to metadata, but as a key/value map */
  get attributes(): Readonly<Record<string, string>> {
    return Object.freeze({ ...this._attributes });
  }

  /** @internal */
  constructor(
    sid: string,
    identity: string,
    name?: string,
    metadata?: string,
    attributes?: Record<string, string>,
    loggerOptions?: LoggerOptions,
    kind: ParticipantKind = ParticipantKind.STANDARD,
  ) {
    super();

    this.log = getLogger(loggerOptions?.loggerName ?? LoggerNames.Participant);
    this.loggerOptions = loggerOptions;

    this.setMaxListeners(100);
    this.sid = sid;
    this.identity = identity;
    this.name = name;
    this.metadata = metadata;
    this.audioTrackPublications = new Map();
    this.videoTrackPublications = new Map();
    this.trackPublications = new Map();
    this._kind = kind;
    this._attributes = attributes ?? {};
  }

  getTrackPublications(): TrackPublication[] {
    return Array.from(this.trackPublications.values());
  }

  /**
   * Finds the first track that matches the source filter, for example, getting
   * the user's camera track with getTrackBySource(Track.Source.Camera).
   */
  getTrackPublication(source: Track.Source): TrackPublication | undefined {
    for (const [, pub] of this.trackPublications) {
      if (pub.source === source) {
        return pub;
      }
    }
  }

  /**
   * Finds the first track that matches the track's name.
   */
  getTrackPublicationByName(name: string): TrackPublication | undefined {
    for (const [, pub] of this.trackPublications) {
      if (pub.trackName === name) {
        return pub;
      }
    }
  }

  /**
   * Waits until the participant is active and ready to receive data messages
   * @returns a promise that resolves when the participant is active
   */
  waitUntilActive(): Promise<void> {
    if (this.isActive) {
      return Promise.resolve();
    }

    if (this.activeFuture) {
      return this.activeFuture.promise;
    }

    this.activeFuture = new Future<void>();

    this.once(ParticipantEvent.Active, () => {
      this.activeFuture?.resolve?.();
      this.activeFuture = undefined;
    });
    return this.activeFuture.promise;
  }

  get connectionQuality(): ConnectionQuality {
    return this._connectionQuality;
  }

  get isCameraEnabled(): boolean {
    const track = this.getTrackPublication(Track.Source.Camera);
    return !(track?.isMuted ?? true);
  }

  get isMicrophoneEnabled(): boolean {
    const track = this.getTrackPublication(Track.Source.Microphone);
    return !(track?.isMuted ?? true);
  }

  get isScreenShareEnabled(): boolean {
    const track = this.getTrackPublication(Track.Source.ScreenShare);
    return !!track;
  }

  get isLocal(): boolean {
    return false;
  }

  /** when participant joined the room */
  get joinedAt(): Date | undefined {
    if (this.participantInfo) {
      return new Date(Number.parseInt(this.participantInfo.joinedAt.toString()) * 1000);
    }
    return new Date();
  }

  /** @internal */
  updateInfo(info: ParticipantInfo): boolean {
    // it's possible the update could be applied out of order due to await
    // during reconnect sequences. when that happens, it's possible for server
    // to have sent more recent version of participant info while JS is waiting
    // to process the existing payload.
    // when the participant sid remains the same, and we already have a later version
    // of the payload, they can be safely skipped
    if (
      this.participantInfo &&
      this.participantInfo.sid === info.sid &&
      this.participantInfo.version > info.version
    ) {
      return false;
    }
    this.identity = info.identity;
    this.sid = info.sid;
    this._setName(info.name);
    this._setMetadata(info.metadata);
    this._setAttributes(info.attributes);
    if (
      info.state === ParticipantInfo_State.ACTIVE &&
      this.participantInfo?.state !== ParticipantInfo_State.ACTIVE
    ) {
      this.emit(ParticipantEvent.Active);
    }
    if (info.permission) {
      this.setPermissions(info.permission);
    }
    // set this last so setMetadata can detect changes
    this.participantInfo = info;
    return true;
  }

  /**
   * Updates metadata from server
   **/
  private _setMetadata(md: string) {
    const changed = this.metadata !== md;
    const prevMetadata = this.metadata;
    this.metadata = md;

    if (changed) {
      this.emit(ParticipantEvent.ParticipantMetadataChanged, prevMetadata);
    }
  }

  private _setName(name: string) {
    const changed = this.name !== name;
    this.name = name;

    if (changed) {
      this.emit(ParticipantEvent.ParticipantNameChanged, name);
    }
  }

  /**
   * Updates metadata from server
   **/
  private _setAttributes(attributes: Record<string, string>) {
    const diff = diffAttributes(this.attributes, attributes);
    this._attributes = attributes;

    if (Object.keys(diff).length > 0) {
      this.emit(ParticipantEvent.AttributesChanged, diff);
    }
  }

  /** @internal */
  setPermissions(permissions: ParticipantPermission): boolean {
    const prevPermissions = this.permissions;
    const changed =
      permissions.canPublish !== this.permissions?.canPublish ||
      permissions.canSubscribe !== this.permissions?.canSubscribe ||
      permissions.canPublishData !== this.permissions?.canPublishData ||
      permissions.hidden !== this.permissions?.hidden ||
      permissions.recorder !== this.permissions?.recorder ||
      permissions.canPublishSources.length !== this.permissions.canPublishSources.length ||
      permissions.canPublishSources.some(
        (value, index) => value !== this.permissions?.canPublishSources[index],
      ) ||
      permissions.canSubscribeMetrics !== this.permissions?.canSubscribeMetrics;
    this.permissions = permissions;

    if (changed) {
      this.emit(ParticipantEvent.ParticipantPermissionsChanged, prevPermissions);
    }
    return changed;
  }

  /** @internal */
  setIsSpeaking(speaking: boolean) {
    if (speaking === this.isSpeaking) {
      return;
    }
    this.isSpeaking = speaking;
    if (speaking) {
      this.lastSpokeAt = new Date();
    }
    this.emit(ParticipantEvent.IsSpeakingChanged, speaking);
  }

  /** @internal */
  setConnectionQuality(q: ProtoQuality) {
    const prevQuality = this._connectionQuality;
    this._connectionQuality = qualityFromProto(q);
    if (prevQuality !== this._connectionQuality) {
      this.emit(ParticipantEvent.ConnectionQualityChanged, this._connectionQuality);
    }
  }

  /**
   * @internal
   */
  setDisconnected() {
    if (this.activeFuture) {
      this.activeFuture.reject?.(new Error('Participant disconnected'));
      this.activeFuture = undefined;
    }
  }

  /**
   * @internal
   */
  setAudioContext(ctx: AudioContext | undefined) {
    this.audioContext = ctx;
    this.audioTrackPublications.forEach(
      (track) => isAudioTrack(track.track) && track.track.setAudioContext(ctx),
    );
  }

  protected addTrackPublication(publication: TrackPublication) {
    // forward publication driven events
    publication.on(TrackEvent.Muted, () => {
      this.emit(ParticipantEvent.TrackMuted, publication);
    });

    publication.on(TrackEvent.Unmuted, () => {
      this.emit(ParticipantEvent.TrackUnmuted, publication);
    });

    const pub = publication;
    if (pub.track) {
      pub.track.sid = publication.trackSid;
    }

    this.trackPublications.set(publication.trackSid, publication);
    switch (publication.kind) {
      case Track.Kind.Audio:
        this.audioTrackPublications.set(publication.trackSid, publication);
        break;
      case Track.Kind.Video:
        this.videoTrackPublications.set(publication.trackSid, publication);
        break;
      default:
        break;
    }
  }
}

export type ParticipantEventCallbacks = {
  trackPublished: (publication: RemoteTrackPublication) => void;
  trackSubscribed: (track: RemoteTrack, publication: RemoteTrackPublication) => void;
  trackSubscriptionFailed: (trackSid: string, reason?: SubscriptionError) => void;
  trackUnpublished: (publication: RemoteTrackPublication) => void;
  trackUnsubscribed: (track: RemoteTrack, publication: RemoteTrackPublication) => void;
  trackMuted: (publication: TrackPublication) => void;
  trackUnmuted: (publication: TrackPublication) => void;
  localTrackPublished: (publication: LocalTrackPublication) => void;
  localTrackUnpublished: (publication: LocalTrackPublication) => void;
  localTrackCpuConstrained: (track: LocalVideoTrack, publication: LocalTrackPublication) => void;
  localSenderCreated: (sender: RTCRtpSender, track: Track) => void;
  participantMetadataChanged: (prevMetadata: string | undefined, participant?: any) => void;
  participantNameChanged: (name: string) => void;
  dataReceived: (
    payload: Uint8Array,
    kind: DataPacket_Kind,
    encryptionType?: Encryption_Type,
  ) => void;
  sipDTMFReceived: (dtmf: SipDTMF) => void;
  transcriptionReceived: (
    transcription: TranscriptionSegment[],
    publication?: TrackPublication,
  ) => void;
  isSpeakingChanged: (speaking: boolean) => void;
  connectionQualityChanged: (connectionQuality: ConnectionQuality) => void;
  trackStreamStateChanged: (
    publication: RemoteTrackPublication,
    streamState: Track.StreamState,
  ) => void;
  trackSubscriptionPermissionChanged: (
    publication: RemoteTrackPublication,
    status: TrackPublication.PermissionStatus,
  ) => void;
  mediaDevicesError: (error: Error, kind?: MediaDeviceKind) => void;
  audioStreamAcquired: () => void;
  participantPermissionsChanged: (prevPermissions?: ParticipantPermission) => void;
  trackSubscriptionStatusChanged: (
    publication: RemoteTrackPublication,
    status: TrackPublication.SubscriptionStatus,
  ) => void;
  attributesChanged: (changedAttributes: Record<string, string>) => void;
  localTrackSubscribed: (trackPublication: LocalTrackPublication) => void;
  chatMessage: (msg: ChatMessage) => void;
  active: () => void;
};
