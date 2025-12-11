import {
  AddTrackRequest,
  AudioTrackFeature,
  BackupCodecPolicy,
  ChatMessage as ChatMessageModel,
  Codec,
  DataPacket,
  DataPacket_Kind,
  Encryption_Type,
  JoinResponse,
  ParticipantInfo,
  RequestResponse,
  RequestResponse_Reason,
  RpcAck,
  RpcRequest,
  RpcResponse,
  SimulcastCodec,
  SipDTMF,
  SubscribedQualityUpdate,
  TrackInfo,
  TrackUnpublishedResponse,
  UserPacket,
  protoInt64,
} from '@livekit/protocol';
import { SignalConnectionState } from '../../api/SignalClient';
import type { InternalRoomOptions } from '../../options';
import { PCTransportState } from '../PCTransportManager';
import type RTCEngine from '../RTCEngine';
import type OutgoingDataStreamManager from '../data-stream/outgoing/OutgoingDataStreamManager';
import type { TextStreamWriter } from '../data-stream/outgoing/StreamWriter';
import { defaultVideoCodec } from '../defaults';
import {
  DeviceUnsupportedError,
  LivekitError,
  PublishTrackError,
  SignalRequestError,
  TrackInvalidError,
  UnexpectedConnectionState,
} from '../errors';
import { EngineEvent, ParticipantEvent, TrackEvent } from '../events';
import {
  MAX_PAYLOAD_BYTES,
  type PerformRpcParams,
  RpcError,
  type RpcInvocationData,
  byteLength,
} from '../rpc';
import LocalAudioTrack from '../track/LocalAudioTrack';
import LocalTrack from '../track/LocalTrack';
import LocalTrackPublication from '../track/LocalTrackPublication';
import LocalVideoTrack, { videoLayersFromEncodings } from '../track/LocalVideoTrack';
import { Track } from '../track/Track';
import { createLocalTracks } from '../track/create';
import type {
  AudioCaptureOptions,
  BackupVideoCodec,
  CreateLocalTracksOptions,
  ScreenShareCaptureOptions,
  TrackPublishOptions,
  VideoCaptureOptions,
} from '../track/options';
import { ScreenSharePresets, VideoPresets, isBackupCodec } from '../track/options';
import {
  getLogContextFromTrack,
  getTrackSourceFromProto,
  mergeDefaultOptions,
  mimeTypeToVideoCodecString,
  screenCaptureToDisplayMediaStreamOptions,
  sourceToKind,
} from '../track/utils';
import {
  type ChatMessage,
  type DataPublishOptions,
  type SendFileOptions,
  type SendTextOptions,
  type StreamBytesOptions,
  type StreamTextOptions,
  type TextStreamInfo,
} from '../types';
import {
  Future,
  compareVersions,
  isAudioTrack,
  isE2EESimulcastSupported,
  isFireFox,
  isLocalAudioTrack,
  isLocalTrack,
  isLocalVideoTrack,
  isSVCCodec,
  isSafari17Based,
  isVideoTrack,
  isWeb,
  sleep,
  supportsAV1,
  supportsVP9,
} from '../utils';
import Participant from './Participant';
import type { ParticipantTrackPermission } from './ParticipantTrackPermission';
import { trackPermissionToProto } from './ParticipantTrackPermission';
import type RemoteParticipant from './RemoteParticipant';
import {
  computeTrackBackupEncodings,
  computeVideoEncodings,
  getDefaultDegradationPreference,
} from './publishUtils';

export default class LocalParticipant extends Participant {
  audioTrackPublications: Map<string, LocalTrackPublication>;

  videoTrackPublications: Map<string, LocalTrackPublication>;

  /** map of track sid => all published tracks */
  trackPublications: Map<string, LocalTrackPublication>;

  /** @internal */
  engine: RTCEngine;

  /** @internal */
  activeDeviceMap: Map<MediaDeviceKind, string>;

  private pendingPublishing = new Set<Track.Source>();

  private pendingPublishPromises = new Map<LocalTrack, Promise<LocalTrackPublication>>();

  private republishPromise: Promise<void> | undefined;

  private cameraError: Error | undefined;

  private microphoneError: Error | undefined;

  private participantTrackPermissions: Array<ParticipantTrackPermission> = [];

  private allParticipantsAllowedToSubscribe: boolean = true;

  // keep a pointer to room options
  private roomOptions: InternalRoomOptions;

  private encryptionType: Encryption_Type = Encryption_Type.NONE;

  private reconnectFuture?: Future<void>;

  private signalConnectedFuture?: Future<void>;

  private activeAgentFuture?: Future<RemoteParticipant>;

  private firstActiveAgent?: RemoteParticipant;

  private rpcHandlers: Map<string, (data: RpcInvocationData) => Promise<string>>;

  private roomOutgoingDataStreamManager: OutgoingDataStreamManager;

  private pendingSignalRequests: Map<
    number,
    {
      resolve: (arg: any) => void;
      reject: (reason: LivekitError) => void;
      values: Partial<Record<keyof LocalParticipant, any>>;
    }
  >;

  private enabledPublishVideoCodecs: Codec[] = [];

  private pendingAcks = new Map<string, { resolve: () => void; participantIdentity: string }>();

  private pendingResponses = new Map<
    string,
    {
      resolve: (payload: string | null, error: RpcError | null) => void;
      participantIdentity: string;
    }
  >();

  /** @internal */
  constructor(
    sid: string,
    identity: string,
    engine: RTCEngine,
    options: InternalRoomOptions,
    roomRpcHandlers: Map<string, (data: RpcInvocationData) => Promise<string>>,
    roomOutgoingDataStreamManager: OutgoingDataStreamManager,
  ) {
    super(sid, identity, undefined, undefined, undefined, {
      loggerName: options.loggerName,
      loggerContextCb: () => this.engine.logContext,
    });
    this.audioTrackPublications = new Map();
    this.videoTrackPublications = new Map();
    this.trackPublications = new Map();
    this.engine = engine;
    this.roomOptions = options;
    this.setupEngine(engine);
    this.activeDeviceMap = new Map([
      ['audioinput', 'default'],
      ['videoinput', 'default'],
      ['audiooutput', 'default'],
    ]);
    this.pendingSignalRequests = new Map();
    this.rpcHandlers = roomRpcHandlers;
    this.roomOutgoingDataStreamManager = roomOutgoingDataStreamManager;
  }

  get lastCameraError(): Error | undefined {
    return this.cameraError;
  }

  get lastMicrophoneError(): Error | undefined {
    return this.microphoneError;
  }

  get isE2EEEnabled(): boolean {
    return this.encryptionType !== Encryption_Type.NONE;
  }

  getTrackPublication(source: Track.Source): LocalTrackPublication | undefined {
    const track = super.getTrackPublication(source);
    if (track) {
      return track as LocalTrackPublication;
    }
  }

  getTrackPublicationByName(name: string): LocalTrackPublication | undefined {
    const track = super.getTrackPublicationByName(name);
    if (track) {
      return track as LocalTrackPublication;
    }
  }

  /**
   * @internal
   */
  setupEngine(engine: RTCEngine) {
    this.engine = engine;
    this.engine.on(EngineEvent.RemoteMute, (trackSid: string, muted: boolean) => {
      const pub = this.trackPublications.get(trackSid);
      if (!pub || !pub.track) {
        return;
      }
      if (muted) {
        pub.mute();
      } else {
        pub.unmute();
      }
    });

    if (this.signalConnectedFuture?.isResolved) {
      this.signalConnectedFuture = undefined;
    }

    this.engine
      .on(EngineEvent.Connected, this.handleReconnected)
      .on(EngineEvent.SignalConnected, this.handleSignalConnected)
      .on(EngineEvent.SignalRestarted, this.handleReconnected)
      .on(EngineEvent.SignalResumed, this.handleReconnected)
      .on(EngineEvent.Restarting, this.handleReconnecting)
      .on(EngineEvent.Resuming, this.handleReconnecting)
      .on(EngineEvent.LocalTrackUnpublished, this.handleLocalTrackUnpublished)
      .on(EngineEvent.SubscribedQualityUpdate, this.handleSubscribedQualityUpdate)
      .on(EngineEvent.Closing, this.handleClosing)
      .on(EngineEvent.SignalRequestResponse, this.handleSignalRequestResponse)
      .on(EngineEvent.DataPacketReceived, this.handleDataPacket);
  }

  private handleReconnecting = () => {
    if (!this.reconnectFuture) {
      this.reconnectFuture = new Future<void>();
    }
  };

  private handleReconnected = () => {
    this.reconnectFuture?.resolve?.();
    this.reconnectFuture = undefined;
    this.updateTrackSubscriptionPermissions();
  };

  private handleClosing = () => {
    if (this.reconnectFuture) {
      this.reconnectFuture.promise.catch((e) => this.log.warn(e.message, this.logContext));
      this.reconnectFuture?.reject?.('Got disconnected during reconnection attempt');
      this.reconnectFuture = undefined;
    }
    if (this.signalConnectedFuture) {
      this.signalConnectedFuture.reject?.('Got disconnected without signal connected');
      this.signalConnectedFuture = undefined;
    }

    this.activeAgentFuture?.reject?.('Got disconnected without active agent present');
    this.activeAgentFuture = undefined;
    this.firstActiveAgent = undefined;
  };

  private handleSignalConnected = (joinResponse: JoinResponse) => {
    if (joinResponse.participant) {
      this.updateInfo(joinResponse.participant);
    }
    if (!this.signalConnectedFuture) {
      this.signalConnectedFuture = new Future<void>();
    }

    this.signalConnectedFuture.resolve?.();
  };

  private handleSignalRequestResponse = (response: RequestResponse) => {
    const { requestId, reason, message } = response;
    const targetRequest = this.pendingSignalRequests.get(requestId);
    if (targetRequest) {
      if (reason !== RequestResponse_Reason.OK) {
        targetRequest.reject(new SignalRequestError(message, reason));
      }
      this.pendingSignalRequests.delete(requestId);
    }
  };

  private handleDataPacket = (packet: DataPacket) => {
    switch (packet.value.case) {
      case 'rpcResponse':
        let rpcResponse = packet.value.value as RpcResponse;
        let payload: string | null = null;
        let error: RpcError | null = null;

        if (rpcResponse.value.case === 'payload') {
          payload = rpcResponse.value.value;
        } else if (rpcResponse.value.case === 'error') {
          error = RpcError.fromProto(rpcResponse.value.value);
        }
        this.handleIncomingRpcResponse(rpcResponse.requestId, payload, error);
        break;
      case 'rpcAck':
        let rpcAck = packet.value.value as RpcAck;
        this.handleIncomingRpcAck(rpcAck.requestId);
        break;
    }
  };

  /**
   * Sets and updates the metadata of the local participant.
   * Note: this requires `canUpdateOwnMetadata` permission.
   * method will throw if the user doesn't have the required permissions
   * @param metadata
   */
  async setMetadata(metadata: string): Promise<void> {
    await this.requestMetadataUpdate({ metadata });
  }

  /**
   * Sets and updates the name of the local participant.
   * Note: this requires `canUpdateOwnMetadata` permission.
   * method will throw if the user doesn't have the required permissions
   * @param metadata
   */
  async setName(name: string): Promise<void> {
    await this.requestMetadataUpdate({ name });
  }

  /**
   * Set or update participant attributes. It will make updates only to keys that
   * are present in `attributes`, and will not override others.
   * Note: this requires `canUpdateOwnMetadata` permission.
   * @param attributes attributes to update
   */
  async setAttributes(attributes: Record<string, string>) {
    await this.requestMetadataUpdate({ attributes });
  }

  private async requestMetadataUpdate({
    metadata,
    name,
    attributes,
  }: {
    metadata?: string;
    name?: string;
    attributes?: Record<string, string>;
  }) {
    return new Promise<void>(async (resolve, reject) => {
      try {
        let isRejected = false;
        const requestId = await this.engine.client.sendUpdateLocalMetadata(
          metadata ?? this.metadata ?? '',
          name ?? this.name ?? '',
          attributes,
        );
        const startTime = performance.now();
        this.pendingSignalRequests.set(requestId, {
          resolve,
          reject: (error: LivekitError) => {
            reject(error);
            isRejected = true;
          },
          values: { name, metadata, attributes },
        });
        while (performance.now() - startTime < 5_000 && !isRejected) {
          if (
            (!name || this.name === name) &&
            (!metadata || this.metadata === metadata) &&
            (!attributes ||
              Object.entries(attributes).every(
                ([key, value]) =>
                  this.attributes[key] === value || (value === '' && !this.attributes[key]),
              ))
          ) {
            this.pendingSignalRequests.delete(requestId);
            resolve();
            return;
          }
          await sleep(50);
        }
        reject(
          new SignalRequestError('Request to update local metadata timed out', 'TimeoutError'),
        );
      } catch (e: any) {
        if (e instanceof Error) reject(e);
      }
    });
  }

  /**
   * Enable or disable a participant's camera track.
   *
   * If a track has already published, it'll mute or unmute the track.
   * Resolves with a `LocalTrackPublication` instance if successful and `undefined` otherwise
   */
  setCameraEnabled(
    enabled: boolean,
    options?: VideoCaptureOptions,
    publishOptions?: TrackPublishOptions,
  ): Promise<LocalTrackPublication | undefined> {
    return this.setTrackEnabled(Track.Source.Camera, enabled, options, publishOptions);
  }

  /**
   * Enable or disable a participant's microphone track.
   *
   * If a track has already published, it'll mute or unmute the track.
   * Resolves with a `LocalTrackPublication` instance if successful and `undefined` otherwise
   */
  setMicrophoneEnabled(
    enabled: boolean,
    options?: AudioCaptureOptions,
    publishOptions?: TrackPublishOptions,
  ): Promise<LocalTrackPublication | undefined> {
    return this.setTrackEnabled(Track.Source.Microphone, enabled, options, publishOptions);
  }

  /**
   * Start or stop sharing a participant's screen
   * Resolves with a `LocalTrackPublication` instance if successful and `undefined` otherwise
   */
  setScreenShareEnabled(
    enabled: boolean,
    options?: ScreenShareCaptureOptions,
    publishOptions?: TrackPublishOptions,
  ): Promise<LocalTrackPublication | undefined> {
    return this.setTrackEnabled(Track.Source.ScreenShare, enabled, options, publishOptions);
  }

  /** @internal */
  async setE2EEEnabled(enabled: boolean) {
    this.encryptionType = enabled ? Encryption_Type.GCM : Encryption_Type.NONE;
    await this.republishAllTracks(undefined, false);
  }

  /**
   * Enable or disable publishing for a track by source. This serves as a simple
   * way to manage the common tracks (camera, mic, or screen share).
   * Resolves with LocalTrackPublication if successful and void otherwise
   */
  private async setTrackEnabled(
    source: Extract<Track.Source, Track.Source.Camera>,
    enabled: boolean,
    options?: VideoCaptureOptions,
    publishOptions?: TrackPublishOptions,
  ): Promise<LocalTrackPublication | undefined>;
  private async setTrackEnabled(
    source: Extract<Track.Source, Track.Source.Microphone>,
    enabled: boolean,
    options?: AudioCaptureOptions,
    publishOptions?: TrackPublishOptions,
  ): Promise<LocalTrackPublication | undefined>;
  private async setTrackEnabled(
    source: Extract<Track.Source, Track.Source.ScreenShare>,
    enabled: boolean,
    options?: ScreenShareCaptureOptions,
    publishOptions?: TrackPublishOptions,
  ): Promise<LocalTrackPublication | undefined>;
  private async setTrackEnabled(
    source: Track.Source,
    enabled: true,
    options?: VideoCaptureOptions | AudioCaptureOptions | ScreenShareCaptureOptions,
    publishOptions?: TrackPublishOptions,
  ) {
    this.log.debug('setTrackEnabled', { ...this.logContext, source, enabled });
    if (this.republishPromise) {
      await this.republishPromise;
    }
    let track = this.getTrackPublication(source);
    if (enabled) {
      if (track) {
        await track.unmute();
      } else {
        let localTracks: Array<LocalTrack> | undefined;
        if (this.pendingPublishing.has(source)) {
          const pendingTrack = await this.waitForPendingPublicationOfSource(source);
          if (!pendingTrack) {
            this.log.info('waiting for pending publication promise timed out', {
              ...this.logContext,
              source,
            });
          }
          await pendingTrack?.unmute();
          return pendingTrack;
        }
        this.pendingPublishing.add(source);
        try {
          switch (source) {
            case Track.Source.Camera:
              localTracks = await this.createTracks({
                video: (options as VideoCaptureOptions | undefined) ?? true,
              });

              break;
            case Track.Source.Microphone:
              localTracks = await this.createTracks({
                audio: (options as AudioCaptureOptions | undefined) ?? true,
              });
              break;
            case Track.Source.ScreenShare:
              localTracks = await this.createScreenTracks({
                ...(options as ScreenShareCaptureOptions | undefined),
              });
              break;
            default:
              throw new TrackInvalidError(source);
          }
        } catch (e: unknown) {
          localTracks?.forEach((tr) => {
            tr.stop();
          });
          if (e instanceof Error) {
            this.emit(ParticipantEvent.MediaDevicesError, e, sourceToKind(source));
          }
          this.pendingPublishing.delete(source);
          throw e;
        }

        for (const localTrack of localTracks) {
          const opts: TrackPublishOptions = {
            ...this.roomOptions.publishDefaults,
            ...options,
          };
          if (
            source === Track.Source.Microphone &&
            isAudioTrack(localTrack) &&
            opts.preConnectBuffer
          ) {
            this.log.info('starting preconnect buffer for microphone', {
              ...this.logContext,
            });
            localTrack.startPreConnectBuffer();
          }
        }

        try {
          const publishPromises: Array<Promise<LocalTrackPublication>> = [];
          for (const localTrack of localTracks) {
            this.log.info('publishing track', {
              ...this.logContext,
              ...getLogContextFromTrack(localTrack),
            });

            publishPromises.push(this.publishTrack(localTrack, publishOptions));
          }
          const publishedTracks = await Promise.all(publishPromises);

          // for screen share publications including audio, this will only return the screen share publication, not the screen share audio one
          // revisit if we want to return an array of tracks instead for v2
          [track] = publishedTracks;
        } catch (e) {
          localTracks?.forEach((tr) => {
            tr.stop();
          });
          throw e;
        } finally {
          this.pendingPublishing.delete(source);
        }
      }
    } else {
      if (!track?.track && this.pendingPublishing.has(source)) {
        // if there's no track available yet first wait for pending publishing promises of that source to see if it becomes available
        track = await this.waitForPendingPublicationOfSource(source);
        if (!track) {
          this.log.info('waiting for pending publication promise timed out', {
            ...this.logContext,
            source,
          });
        }
      }
      if (track && track.track) {
        // screenshare cannot be muted, unpublish instead
        if (source === Track.Source.ScreenShare) {
          track = await this.unpublishTrack(track.track);
          const screenAudioTrack = this.getTrackPublication(Track.Source.ScreenShareAudio);
          if (screenAudioTrack && screenAudioTrack.track) {
            this.unpublishTrack(screenAudioTrack.track);
          }
        } else {
          await track.mute();
        }
      }
    }
    return track;
  }

  /**
   * Publish both camera and microphone at the same time. This is useful for
   * displaying a single Permission Dialog box to the end user.
   */
  async enableCameraAndMicrophone() {
    if (
      this.pendingPublishing.has(Track.Source.Camera) ||
      this.pendingPublishing.has(Track.Source.Microphone)
    ) {
      // no-op it's already been requested
      return;
    }

    this.pendingPublishing.add(Track.Source.Camera);
    this.pendingPublishing.add(Track.Source.Microphone);
    try {
      const tracks: LocalTrack[] = await this.createTracks({
        audio: true,
        video: true,
      });

      await Promise.all(tracks.map((track) => this.publishTrack(track)));
    } finally {
      this.pendingPublishing.delete(Track.Source.Camera);
      this.pendingPublishing.delete(Track.Source.Microphone);
    }
  }

  /**
   * Create local camera and/or microphone tracks
   * @param options
   * @returns
   */
  async createTracks(options?: CreateLocalTracksOptions): Promise<LocalTrack[]> {
    options ??= {};

    const mergedOptionsWithProcessors = mergeDefaultOptions(
      options,
      this.roomOptions?.audioCaptureDefaults,
      this.roomOptions?.videoCaptureDefaults,
    );

    try {
      const tracks = await createLocalTracks(mergedOptionsWithProcessors, {
        loggerName: this.roomOptions.loggerName,
        loggerContextCb: () => this.logContext,
      });
      const localTracks = tracks.map((track) => {
        if (isAudioTrack(track)) {
          this.microphoneError = undefined;
          track.setAudioContext(this.audioContext);
          track.source = Track.Source.Microphone;
          this.emit(ParticipantEvent.AudioStreamAcquired);
        }
        if (isVideoTrack(track)) {
          this.cameraError = undefined;
          track.source = Track.Source.Camera;
        }
        return track;
      });
      return localTracks;
    } catch (err) {
      if (err instanceof Error) {
        if (options.audio) {
          this.microphoneError = err;
        }
        if (options.video) {
          this.cameraError = err;
        }
      }

      throw err;
    }
  }

  /**
   * Creates a screen capture tracks with getDisplayMedia().
   * A LocalVideoTrack is always created and returned.
   * If { audio: true }, and the browser supports audio capture, a LocalAudioTrack is also created.
   */
  async createScreenTracks(options?: ScreenShareCaptureOptions): Promise<Array<LocalTrack>> {
    if (options === undefined) {
      options = {};
    }

    if (navigator.mediaDevices.getDisplayMedia === undefined) {
      throw new DeviceUnsupportedError('getDisplayMedia not supported');
    }

    if (options.resolution === undefined && !isSafari17Based()) {
      // we need to constrain the dimensions, otherwise it could lead to low bitrate
      // due to encoding a huge video. Encoding such large surfaces is really expensive
      // unfortunately Safari 17 has a but and cannot be constrained by default
      options.resolution = ScreenSharePresets.h1080fps30.resolution;
    }

    const constraints = screenCaptureToDisplayMediaStreamOptions(options);
    const stream: MediaStream = await navigator.mediaDevices.getDisplayMedia(constraints);

    const tracks = stream.getVideoTracks();
    if (tracks.length === 0) {
      throw new TrackInvalidError('no video track found');
    }
    const screenVideo = new LocalVideoTrack(tracks[0], undefined, false, {
      loggerName: this.roomOptions.loggerName,
      loggerContextCb: () => this.logContext,
    });
    screenVideo.source = Track.Source.ScreenShare;
    if (options.contentHint) {
      screenVideo.mediaStreamTrack.contentHint = options.contentHint;
    }

    const localTracks: Array<LocalTrack> = [screenVideo];
    if (stream.getAudioTracks().length > 0) {
      this.emit(ParticipantEvent.AudioStreamAcquired);
      const screenAudio = new LocalAudioTrack(
        stream.getAudioTracks()[0],
        undefined,
        false,
        this.audioContext,
        { loggerName: this.roomOptions.loggerName, loggerContextCb: () => this.logContext },
      );
      screenAudio.source = Track.Source.ScreenShareAudio;
      localTracks.push(screenAudio);
    }
    return localTracks;
  }

  /**
   * Publish a new track to the room
   * @param track
   * @param options
   */
  async publishTrack(track: LocalTrack | MediaStreamTrack, options?: TrackPublishOptions) {
    return this.publishOrRepublishTrack(track, options);
  }

  private async publishOrRepublishTrack(
    track: LocalTrack | MediaStreamTrack,
    options?: TrackPublishOptions,
    isRepublish = false,
  ): Promise<LocalTrackPublication> {
    if (isLocalAudioTrack(track)) {
      track.setAudioContext(this.audioContext);
    }

    await this.reconnectFuture?.promise;
    if (this.republishPromise && !isRepublish) {
      await this.republishPromise;
    }
    if (isLocalTrack(track) && this.pendingPublishPromises.has(track)) {
      await this.pendingPublishPromises.get(track);
    }
    let defaultConstraints: MediaTrackConstraints | undefined;
    if (track instanceof MediaStreamTrack) {
      defaultConstraints = track.getConstraints();
    } else {
      // we want to access constraints directly as `track.mediaStreamTrack`
      // might be pointing to a non-device track (e.g. processed track) already
      defaultConstraints = track.constraints;
      let deviceKind: MediaDeviceKind | undefined = undefined;
      switch (track.source) {
        case Track.Source.Microphone:
          deviceKind = 'audioinput';
          break;
        case Track.Source.Camera:
          deviceKind = 'videoinput';
        default:
          break;
      }
      if (deviceKind && this.activeDeviceMap.has(deviceKind)) {
        defaultConstraints = {
          ...defaultConstraints,
          deviceId: this.activeDeviceMap.get(deviceKind),
        };
      }
    }
    // convert raw media track into audio or video track
    if (track instanceof MediaStreamTrack) {
      switch (track.kind) {
        case 'audio':
          track = new LocalAudioTrack(track, defaultConstraints, true, this.audioContext, {
            loggerName: this.roomOptions.loggerName,
            loggerContextCb: () => this.logContext,
          });
          break;
        case 'video':
          track = new LocalVideoTrack(track, defaultConstraints, true, {
            loggerName: this.roomOptions.loggerName,
            loggerContextCb: () => this.logContext,
          });
          break;
        default:
          throw new TrackInvalidError(`unsupported MediaStreamTrack kind ${track.kind}`);
      }
    } else {
      track.updateLoggerOptions({
        loggerName: this.roomOptions.loggerName,
        loggerContextCb: () => this.logContext,
      });
    }

    // is it already published? if so skip
    let existingPublication: LocalTrackPublication | undefined;
    this.trackPublications.forEach((publication) => {
      if (!publication.track) {
        return;
      }
      if (publication.track === track) {
        existingPublication = <LocalTrackPublication>publication;
      }
    });

    if (existingPublication) {
      this.log.warn('track has already been published, skipping', {
        ...this.logContext,
        ...getLogContextFromTrack(existingPublication),
      });
      return existingPublication;
    }

    const opts: TrackPublishOptions = {
      ...this.roomOptions.publishDefaults,
      ...options,
    };
    const isStereoInput =
      ('channelCount' in track.mediaStreamTrack.getSettings() &&
        // @ts-ignore `channelCount` on getSettings() is currently only available for Safari, but is generally the best way to determine a stereo track https://developer.mozilla.org/en-US/docs/Web/API/MediaTrackSettings/channelCount
        track.mediaStreamTrack.getSettings().channelCount === 2) ||
      track.mediaStreamTrack.getConstraints().channelCount === 2;
    const isStereo = opts.forceStereo ?? isStereoInput;

    // disable dtx for stereo track if not enabled explicitly
    if (isStereo) {
      if (opts.dtx === undefined) {
        this.log.info(
          `Opus DTX will be disabled for stereo tracks by default. Enable them explicitly to make it work.`,
          {
            ...this.logContext,
            ...getLogContextFromTrack(track),
          },
        );
      }
      if (opts.red === undefined) {
        this.log.info(
          `Opus RED will be disabled for stereo tracks by default. Enable them explicitly to make it work.`,
        );
      }
      opts.dtx ??= false;
      opts.red ??= false;
    }

    if (!isE2EESimulcastSupported() && this.roomOptions.e2ee) {
      this.log.info(
        `End-to-end encryption is set up, simulcast publishing will be disabled on Safari versions and iOS browsers running iOS < v17.2`,
        {
          ...this.logContext,
        },
      );
      opts.simulcast = false;
    }

    if (opts.source) {
      track.source = opts.source;
    }
    const publishPromise = new Promise<LocalTrackPublication>(async (resolve, reject) => {
      try {
        if (this.engine.client.currentState !== SignalConnectionState.CONNECTED) {
          this.log.debug('deferring track publication until signal is connected', {
            ...this.logContext,
            track: getLogContextFromTrack(track),
          });

          const timeout = setTimeout(() => {
            reject(
              new PublishTrackError(
                'publishing rejected as engine not connected within timeout',
                408,
              ),
            );
          }, 15_000);
          await this.waitUntilEngineConnected();
          clearTimeout(timeout);
          const publication = await this.publish(track, opts, isStereo);
          resolve(publication);
        } else {
          try {
            const publication = await this.publish(track, opts, isStereo);
            resolve(publication);
          } catch (e) {
            reject(e);
          }
        }
      } catch (e) {
        reject(e);
      }
    });
    this.pendingPublishPromises.set(track, publishPromise);
    try {
      const publication = await publishPromise;
      return publication;
    } catch (e) {
      throw e;
    } finally {
      this.pendingPublishPromises.delete(track);
    }
  }

  private waitUntilEngineConnected() {
    if (!this.signalConnectedFuture) {
      this.signalConnectedFuture = new Future<void>();
    }
    return this.signalConnectedFuture.promise;
  }

  private hasPermissionsToPublish(track: LocalTrack): boolean {
    if (!this.permissions) {
      this.log.warn('no permissions present for publishing track', {
        ...this.logContext,
        ...getLogContextFromTrack(track),
      });
      return false;
    }
    const { canPublish, canPublishSources } = this.permissions;
    if (
      canPublish &&
      (canPublishSources.length === 0 ||
        canPublishSources.map((source) => getTrackSourceFromProto(source)).includes(track.source))
    ) {
      return true;
    }
    this.log.warn('insufficient permissions to publish', {
      ...this.logContext,
      ...getLogContextFromTrack(track),
    });
    return false;
  }

  private async publish(track: LocalTrack, opts: TrackPublishOptions, isStereo: boolean) {
    if (!this.hasPermissionsToPublish(track)) {
      throw new PublishTrackError('failed to publish track, insufficient permissions', 403);
    }
    const existingTrackOfSource = Array.from(this.trackPublications.values()).find(
      (publishedTrack) => isLocalTrack(track) && publishedTrack.source === track.source,
    );
    if (existingTrackOfSource && track.source !== Track.Source.Unknown) {
      this.log.info(`publishing a second track with the same source: ${track.source}`, {
        ...this.logContext,
        ...getLogContextFromTrack(track),
      });
    }
    if (opts.stopMicTrackOnMute && isAudioTrack(track)) {
      track.stopOnMute = true;
    }

    if (track.source === Track.Source.ScreenShare && isFireFox()) {
      // Firefox does not work well with simulcasted screen share
      // we frequently get no data on layer 0 when enabled
      opts.simulcast = false;
    }

    // require full AV1/VP9 SVC support prior to using it
    if (opts.videoCodec === 'av1' && !supportsAV1()) {
      opts.videoCodec = undefined;
    }
    if (opts.videoCodec === 'vp9' && !supportsVP9()) {
      opts.videoCodec = undefined;
    }
    if (opts.videoCodec === undefined) {
      opts.videoCodec = defaultVideoCodec;
    }
    if (this.enabledPublishVideoCodecs.length > 0) {
      // fallback to a supported codec if it is not supported
      if (
        !this.enabledPublishVideoCodecs.some(
          (c) => opts.videoCodec === mimeTypeToVideoCodecString(c.mime),
        )
      ) {
        opts.videoCodec = mimeTypeToVideoCodecString(this.enabledPublishVideoCodecs[0].mime);
      }
    }

    const videoCodec = opts.videoCodec;

    // handle track actions
    track.on(TrackEvent.Muted, this.onTrackMuted);
    track.on(TrackEvent.Unmuted, this.onTrackUnmuted);
    track.on(TrackEvent.Ended, this.handleTrackEnded);
    track.on(TrackEvent.UpstreamPaused, this.onTrackUpstreamPaused);
    track.on(TrackEvent.UpstreamResumed, this.onTrackUpstreamResumed);
    track.on(TrackEvent.AudioTrackFeatureUpdate, this.onTrackFeatureUpdate);

    const audioFeatures: AudioTrackFeature[] = [];
    const disableDtx = !(opts.dtx ?? true);

    const settings = track.getSourceTrackSettings();

    if (settings.autoGainControl) {
      audioFeatures.push(AudioTrackFeature.TF_AUTO_GAIN_CONTROL);
    }
    if (settings.echoCancellation) {
      audioFeatures.push(AudioTrackFeature.TF_ECHO_CANCELLATION);
    }
    if (settings.noiseSuppression) {
      audioFeatures.push(AudioTrackFeature.TF_NOISE_SUPPRESSION);
    }
    if (settings.channelCount && settings.channelCount > 1) {
      audioFeatures.push(AudioTrackFeature.TF_STEREO);
    }
    if (disableDtx) {
      audioFeatures.push(AudioTrackFeature.TF_NO_DTX);
    }
    if (isLocalAudioTrack(track) && track.hasPreConnectBuffer) {
      audioFeatures.push(AudioTrackFeature.TF_PRECONNECT_BUFFER);
    }

    // create track publication from track
    const req = new AddTrackRequest({
      // get local track id for use during publishing
      cid: track.mediaStreamTrack.id,
      name: opts.name,
      type: Track.kindToProto(track.kind),
      muted: track.isMuted,
      source: Track.sourceToProto(track.source),
      disableDtx,
      encryption: this.encryptionType,
      stereo: isStereo,
      disableRed: this.isE2EEEnabled || !(opts.red ?? true),
      stream: opts?.stream,
      backupCodecPolicy: opts?.backupCodecPolicy as BackupCodecPolicy,
      audioFeatures,
    });

    // compute encodings and layers for video
    let encodings: RTCRtpEncodingParameters[] | undefined;
    if (track.kind === Track.Kind.Video) {
      let dims: Track.Dimensions = {
        width: 0,
        height: 0,
      };
      try {
        dims = await track.waitForDimensions();
      } catch (e) {
        // use defaults, it's quite painful for congestion control without simulcast
        // so using default dims according to publish settings
        const defaultRes =
          this.roomOptions.videoCaptureDefaults?.resolution ?? VideoPresets.h720.resolution;
        dims = {
          width: defaultRes.width,
          height: defaultRes.height,
        };
        // log failure
        this.log.error('could not determine track dimensions, using defaults', {
          ...this.logContext,
          ...getLogContextFromTrack(track),
          dims,
        });
      }
      // width and height should be defined for video
      req.width = dims.width;
      req.height = dims.height;
      // for svc codecs, disable simulcast and use vp8 for backup codec
      if (isLocalVideoTrack(track)) {
        if (isSVCCodec(videoCodec)) {
          if (track.source === Track.Source.ScreenShare) {
            // vp9 svc with screenshare cannot encode multiple spatial layers
            // doing so reduces publish resolution to minimal resolution
            opts.scalabilityMode = 'L1T3';
            // Chrome does not allow more than 5 fps with L1T3, and it has encoding bugs with L3T3
            // It has a different path for screenshare handling and it seems to be untested/buggy
            // As a workaround, we are setting contentHint to force it to go through the same
            // path as regular camera video. While this is not optimal, it delivers the performance
            // that we need
            if ('contentHint' in track.mediaStreamTrack) {
              track.mediaStreamTrack.contentHint = 'motion';
              this.log.info('forcing contentHint to motion for screenshare with SVC codecs', {
                ...this.logContext,
                ...getLogContextFromTrack(track),
              });
            }
          }
          // set scalabilityMode to 'L3T3_KEY' by default
          opts.scalabilityMode = opts.scalabilityMode ?? 'L3T3_KEY';
        }

        req.simulcastCodecs = [
          new SimulcastCodec({
            codec: videoCodec,
            cid: track.mediaStreamTrack.id,
          }),
        ];

        // set up backup
        if (opts.backupCodec === true) {
          opts.backupCodec = { codec: defaultVideoCodec };
        }
        if (
          opts.backupCodec &&
          videoCodec !== opts.backupCodec.codec &&
          // TODO remove this once e2ee is supported for backup codecs
          req.encryption === Encryption_Type.NONE
        ) {
          // multi-codec simulcast requires dynacast
          if (!this.roomOptions.dynacast) {
            this.roomOptions.dynacast = true;
          }
          req.simulcastCodecs.push(
            new SimulcastCodec({
              codec: opts.backupCodec.codec,
              cid: '',
            }),
          );
        }
      }

      encodings = computeVideoEncodings(
        track.source === Track.Source.ScreenShare,
        req.width,
        req.height,
        opts,
      );
      req.layers = videoLayersFromEncodings(
        req.width,
        req.height,
        encodings,
        isSVCCodec(opts.videoCodec),
      );
    } else if (track.kind === Track.Kind.Audio) {
      encodings = [
        {
          maxBitrate: opts.audioPreset?.maxBitrate,
          priority: opts.audioPreset?.priority ?? 'high',
          networkPriority: opts.audioPreset?.priority ?? 'high',
        },
      ];
    }

    if (!this.engine || this.engine.isClosed) {
      throw new UnexpectedConnectionState('cannot publish track when not connected');
    }

    const negotiate = async () => {
      if (!this.engine.pcManager) {
        throw new UnexpectedConnectionState('pcManager is not ready');
      }

      track.sender = await this.engine.createSender(track, opts, encodings);
      this.emit(ParticipantEvent.LocalSenderCreated, track.sender, track);

      if (isLocalVideoTrack(track)) {
        opts.degradationPreference ??= getDefaultDegradationPreference(track);
        track.setDegradationPreference(opts.degradationPreference);
      }

      if (encodings) {
        if (isFireFox() && track.kind === Track.Kind.Audio) {
          /* Refer to RFC https://datatracker.ietf.org/doc/html/rfc7587#section-6.1,
             livekit-server uses maxaveragebitrate=510000 in the answer sdp to permit client to
             publish high quality audio track. But firefox always uses this value as the actual
             bitrates, causing the audio bitrates to rise to 510Kbps in any stereo case unexpectedly.
             So the client need to modify maxaverragebitrates in answer sdp to user provided value to
             fix the issue.
           */
          let trackTransceiver: RTCRtpTransceiver | undefined = undefined;
          for (const transceiver of this.engine.pcManager.publisher.getTransceivers()) {
            if (transceiver.sender === track.sender) {
              trackTransceiver = transceiver;
              break;
            }
          }
          if (trackTransceiver) {
            this.engine.pcManager.publisher.setTrackCodecBitrate({
              transceiver: trackTransceiver,
              codec: 'opus',
              maxbr: encodings[0]?.maxBitrate ? encodings[0].maxBitrate / 1000 : 0,
            });
          }
        } else if (track.codec && isSVCCodec(track.codec) && encodings[0]?.maxBitrate) {
          this.engine.pcManager.publisher.setTrackCodecBitrate({
            cid: req.cid,
            codec: track.codec,
            maxbr: encodings[0].maxBitrate / 1000,
          });
        }
      }

      await this.engine.negotiate();
    };

    let ti: TrackInfo;
    const addTrackPromise = new Promise<TrackInfo>(async (resolve, reject) => {
      try {
        ti = await this.engine.addTrack(req);
        resolve(ti);
      } catch (err) {
        if (track.sender && this.engine.pcManager?.publisher) {
          this.engine.pcManager.publisher.removeTrack(track.sender);
          await this.engine.negotiate().catch((negotiateErr) => {
            this.log.error(
              'failed to negotiate after removing track due to failed add track request',
              {
                ...this.logContext,
                ...getLogContextFromTrack(track),
                error: negotiateErr,
              },
            );
          });
        }
        reject(err);
      }
    });
    if (this.enabledPublishVideoCodecs.length > 0) {
      const rets = await Promise.all([addTrackPromise, negotiate()]);
      ti = rets[0];
    } else {
      ti = await addTrackPromise;
      // server might not support the codec the client has requested, in that case, fallback
      // to a supported codec
      let primaryCodecMime: string | undefined;
      ti.codecs.forEach((codec) => {
        if (primaryCodecMime === undefined) {
          primaryCodecMime = codec.mimeType;
        }
      });
      if (primaryCodecMime && track.kind === Track.Kind.Video) {
        const updatedCodec = mimeTypeToVideoCodecString(primaryCodecMime);
        if (updatedCodec !== videoCodec) {
          this.log.debug('falling back to server selected codec', {
            ...this.logContext,
            ...getLogContextFromTrack(track),
            codec: updatedCodec,
          });
          opts.videoCodec = updatedCodec;

          // recompute encodings since bitrates/etc could have changed
          encodings = computeVideoEncodings(
            track.source === Track.Source.ScreenShare,
            req.width,
            req.height,
            opts,
          );
        }
      }
      await negotiate();
    }

    const publication = new LocalTrackPublication(track.kind, ti, track, {
      loggerName: this.roomOptions.loggerName,
      loggerContextCb: () => this.logContext,
    });
    publication.on(TrackEvent.CpuConstrained, (constrainedTrack) =>
      this.onTrackCpuConstrained(constrainedTrack, publication),
    );
    // save options for when it needs to be republished again
    publication.options = opts;
    track.sid = ti.sid;

    this.log.debug(`publishing ${track.kind} with encodings`, {
      ...this.logContext,
      encodings,
      trackInfo: ti,
    });

    if (isLocalVideoTrack(track)) {
      track.startMonitor(this.engine.client);
    } else if (isLocalAudioTrack(track)) {
      track.startMonitor();
    }

    this.addTrackPublication(publication);
    // send event for publication
    this.emit(ParticipantEvent.LocalTrackPublished, publication);

    if (
      isLocalAudioTrack(track) &&
      ti.audioFeatures.includes(AudioTrackFeature.TF_PRECONNECT_BUFFER)
    ) {
      const stream = track.getPreConnectBuffer();
      const mimeType = track.getPreConnectBufferMimeType();
      // TODO: we're registering the listener after negotiation, so there might be a race
      this.on(ParticipantEvent.LocalTrackSubscribed, (pub) => {
        if (pub.trackSid === ti.sid) {
          if (!track.hasPreConnectBuffer) {
            this.log.warn('subscribe event came to late, buffer already closed', this.logContext);
            return;
          }
          this.log.debug('finished recording preconnect buffer', {
            ...this.logContext,
            ...getLogContextFromTrack(track),
          });
          track.stopPreConnectBuffer();
        }
      });

      if (stream) {
        const bufferStreamPromise = new Promise<void>(async (resolve, reject) => {
          try {
            this.log.debug('waiting for agent', {
              ...this.logContext,
              ...getLogContextFromTrack(track),
            });
            const agentActiveTimeout = setTimeout(() => {
              reject(new Error('agent not active within 10 seconds'));
            }, 10_000);
            const agent = await this.waitUntilActiveAgentPresent();
            clearTimeout(agentActiveTimeout);
            this.log.debug('sending preconnect buffer', {
              ...this.logContext,
              ...getLogContextFromTrack(track),
            });
            const writer = await this.streamBytes({
              name: 'preconnect-buffer',
              mimeType,
              topic: 'lk.agent.pre-connect-audio-buffer',
              destinationIdentities: [agent.identity],
              attributes: {
                trackId: publication.trackSid,
                sampleRate: String(settings.sampleRate ?? '48000'),
                channels: String(settings.channelCount ?? '1'),
              },
            });
            for await (const chunk of stream) {
              await writer.write(chunk);
            }
            await writer.close();
            resolve();
          } catch (e) {
            reject(e);
          }
        });
        bufferStreamPromise
          .then(() => {
            this.log.debug('preconnect buffer sent successfully', {
              ...this.logContext,
              ...getLogContextFromTrack(track),
            });
          })
          .catch((e) => {
            this.log.error('error sending preconnect buffer', {
              ...this.logContext,
              ...getLogContextFromTrack(track),
              error: e,
            });
          });
      }
    }
    return publication;
  }

  override get isLocal(): boolean {
    return true;
  }

  /** @internal
   * publish additional codec to existing track
   */
  async publishAdditionalCodecForTrack(
    track: LocalTrack | MediaStreamTrack,
    videoCodec: BackupVideoCodec,
    options?: TrackPublishOptions,
  ) {
    // TODO remove once e2ee is supported for backup tracks
    if (this.encryptionType !== Encryption_Type.NONE) {
      return;
    }

    // is it not published? if so skip
    let existingPublication: LocalTrackPublication | undefined;
    this.trackPublications.forEach((publication) => {
      if (!publication.track) {
        return;
      }
      if (publication.track === track) {
        existingPublication = <LocalTrackPublication>publication;
      }
    });
    if (!existingPublication) {
      throw new TrackInvalidError('track is not published');
    }

    if (!isLocalVideoTrack(track)) {
      throw new TrackInvalidError('track is not a video track');
    }

    const opts: TrackPublishOptions = {
      ...this.roomOptions?.publishDefaults,
      ...options,
    };

    const encodings = computeTrackBackupEncodings(track, videoCodec, opts);
    if (!encodings) {
      this.log.info(
        `backup codec has been disabled, ignoring request to add additional codec for track`,
        {
          ...this.logContext,
          ...getLogContextFromTrack(track),
        },
      );
      return;
    }
    const simulcastTrack = track.addSimulcastTrack(videoCodec, encodings);
    if (!simulcastTrack) {
      return;
    }
    const req = new AddTrackRequest({
      cid: simulcastTrack.mediaStreamTrack.id,
      type: Track.kindToProto(track.kind),
      muted: track.isMuted,
      source: Track.sourceToProto(track.source),
      sid: track.sid,
      simulcastCodecs: [
        {
          codec: opts.videoCodec,
          cid: simulcastTrack.mediaStreamTrack.id,
        },
      ],
    });
    req.layers = videoLayersFromEncodings(req.width, req.height, encodings);

    if (!this.engine || this.engine.isClosed) {
      throw new UnexpectedConnectionState('cannot publish track when not connected');
    }

    const negotiate = async () => {
      const transceiverInit: RTCRtpTransceiverInit = { direction: 'sendonly' };
      if (encodings) {
        transceiverInit.sendEncodings = encodings;
      }
      await this.engine.createSimulcastSender(track, simulcastTrack, opts, encodings);

      await this.engine.negotiate();
    };

    const rets = await Promise.all([this.engine.addTrack(req), negotiate()]);
    const ti = rets[0];

    this.log.debug(`published ${videoCodec} for track ${track.sid}`, {
      ...this.logContext,
      encodings,
      trackInfo: ti,
    });
  }

  async unpublishTrack(
    track: LocalTrack | MediaStreamTrack,
    stopOnUnpublish?: boolean,
  ): Promise<LocalTrackPublication | undefined> {
    if (isLocalTrack(track)) {
      const publishPromise = this.pendingPublishPromises.get(track);
      if (publishPromise) {
        this.log.info('awaiting publish promise before attempting to unpublish', {
          ...this.logContext,
          ...getLogContextFromTrack(track),
        });
        await publishPromise;
      }
    }
    // look through all published tracks to find the right ones
    const publication = this.getPublicationForTrack(track);

    const pubLogContext = publication ? getLogContextFromTrack(publication) : undefined;

    this.log.debug('unpublishing track', {
      ...this.logContext,
      ...pubLogContext,
    });

    if (!publication || !publication.track) {
      this.log.warn('track was not unpublished because no publication was found', {
        ...this.logContext,
        ...pubLogContext,
      });
      return undefined;
    }

    track = publication.track;
    track.off(TrackEvent.Muted, this.onTrackMuted);
    track.off(TrackEvent.Unmuted, this.onTrackUnmuted);
    track.off(TrackEvent.Ended, this.handleTrackEnded);
    track.off(TrackEvent.UpstreamPaused, this.onTrackUpstreamPaused);
    track.off(TrackEvent.UpstreamResumed, this.onTrackUpstreamResumed);
    track.off(TrackEvent.AudioTrackFeatureUpdate, this.onTrackFeatureUpdate);

    if (stopOnUnpublish === undefined) {
      stopOnUnpublish = this.roomOptions?.stopLocalTrackOnUnpublish ?? true;
    }
    if (stopOnUnpublish) {
      track.stop();
    } else {
      track.stopMonitor();
    }

    let negotiationNeeded = false;
    const trackSender = track.sender;
    track.sender = undefined;
    if (
      this.engine.pcManager &&
      this.engine.pcManager.currentState < PCTransportState.FAILED &&
      trackSender
    ) {
      try {
        for (const transceiver of this.engine.pcManager.publisher.getTransceivers()) {
          // if sender is not currently sending (after replaceTrack(null))
          // removeTrack would have no effect.
          // to ensure we end up successfully removing the track, manually set
          // the transceiver to inactive
          if (transceiver.sender === trackSender) {
            transceiver.direction = 'inactive';
            negotiationNeeded = true;
          }
        }
        if (this.engine.removeTrack(trackSender)) {
          negotiationNeeded = true;
        }
        if (isLocalVideoTrack(track)) {
          for (const [, trackInfo] of track.simulcastCodecs) {
            if (trackInfo.sender) {
              if (this.engine.removeTrack(trackInfo.sender)) {
                negotiationNeeded = true;
              }
              trackInfo.sender = undefined;
            }
          }
          track.simulcastCodecs.clear();
        }
      } catch (e) {
        this.log.warn('failed to unpublish track', {
          ...this.logContext,
          ...pubLogContext,
          error: e,
        });
      }
    }

    // remove from our maps
    this.trackPublications.delete(publication.trackSid);
    switch (publication.kind) {
      case Track.Kind.Audio:
        this.audioTrackPublications.delete(publication.trackSid);
        break;
      case Track.Kind.Video:
        this.videoTrackPublications.delete(publication.trackSid);
        break;
      default:
        break;
    }

    this.emit(ParticipantEvent.LocalTrackUnpublished, publication);
    publication.setTrack(undefined);

    if (negotiationNeeded) {
      await this.engine.negotiate();
    }
    return publication;
  }

  async unpublishTracks(
    tracks: LocalTrack[] | MediaStreamTrack[],
  ): Promise<LocalTrackPublication[]> {
    const results = await Promise.all(tracks.map((track) => this.unpublishTrack(track)));
    return results.filter((track) => !!track);
  }

  async republishAllTracks(options?: TrackPublishOptions, restartTracks: boolean = true) {
    if (this.republishPromise) {
      await this.republishPromise;
    }
    this.republishPromise = new Promise(async (resolve, reject) => {
      try {
        const localPubs: LocalTrackPublication[] = [];
        this.trackPublications.forEach((pub) => {
          if (pub.track) {
            if (options) {
              pub.options = { ...pub.options, ...options };
            }
            localPubs.push(pub);
          }
        });

        await Promise.all(
          localPubs.map(async (pub) => {
            const track = pub.track!;
            await this.unpublishTrack(track, false);
            if (
              restartTracks &&
              !track.isMuted &&
              track.source !== Track.Source.ScreenShare &&
              track.source !== Track.Source.ScreenShareAudio &&
              (isLocalAudioTrack(track) || isLocalVideoTrack(track)) &&
              !track.isUserProvided
            ) {
              // generally we need to restart the track before publishing, often a full reconnect
              // is necessary because computer had gone to sleep.
              this.log.debug('restarting existing track', {
                ...this.logContext,
                track: pub.trackSid,
              });
              await track.restartTrack();
            }
            await this.publishOrRepublishTrack(track, pub.options, true);
          }),
        );
        resolve();
      } catch (error: any) {
        reject(error);
      } finally {
        this.republishPromise = undefined;
      }
    });

    await this.republishPromise;
  }

  /**
   * Publish a new data payload to the room. Data will be forwarded to each
   * participant in the room if the destination field in publishOptions is empty
   *
   * @param data Uint8Array of the payload. To send string data, use TextEncoder.encode
   * @param options optionally specify a `reliable`, `topic` and `destination`
   */
  async publishData(data: Uint8Array, options: DataPublishOptions = {}): Promise<void> {
    const kind = options.reliable ? DataPacket_Kind.RELIABLE : DataPacket_Kind.LOSSY;
    const destinationIdentities = options.destinationIdentities;
    const topic = options.topic;

    let userPacket = new UserPacket({
      participantIdentity: this.identity,
      payload: data,
      destinationIdentities,
      topic,
    });

    const packet = new DataPacket({
      kind: kind,
      value: {
        case: 'user',
        value: userPacket,
      },
    });

    await this.engine.sendDataPacket(packet, kind);
  }

  /**
   * Publish SIP DTMF message to the room.
   *
   * @param code DTMF code
   * @param digit DTMF digit
   */
  async publishDtmf(code: number, digit: string): Promise<void> {
    const packet = new DataPacket({
      kind: DataPacket_Kind.RELIABLE,
      value: {
        case: 'sipDtmf',
        value: new SipDTMF({
          code: code,
          digit: digit,
        }),
      },
    });

    await this.engine.sendDataPacket(packet, DataPacket_Kind.RELIABLE);
  }

  /** @deprecated Consider migrating to {@link sendText} */
  async sendChatMessage(text: string, options?: SendTextOptions): Promise<ChatMessage> {
    const msg = {
      id: crypto.randomUUID(),
      message: text,
      timestamp: Date.now(),
      attachedFiles: options?.attachments,
    } as const satisfies ChatMessage;
    const packet = new DataPacket({
      value: {
        case: 'chatMessage',
        value: new ChatMessageModel({
          ...msg,
          timestamp: protoInt64.parse(msg.timestamp),
        }),
      },
    });
    await this.engine.sendDataPacket(packet, DataPacket_Kind.RELIABLE);

    this.emit(ParticipantEvent.ChatMessage, msg);
    return msg;
  }

  /** @deprecated Consider migrating to {@link sendText} */
  async editChatMessage(editText: string, originalMessage: ChatMessage) {
    const msg = {
      ...originalMessage,
      message: editText,
      editTimestamp: Date.now(),
    } as const satisfies ChatMessage;
    const packet = new DataPacket({
      value: {
        case: 'chatMessage',
        value: new ChatMessageModel({
          ...msg,
          timestamp: protoInt64.parse(msg.timestamp),
          editTimestamp: protoInt64.parse(msg.editTimestamp),
        }),
      },
    });
    await this.engine.sendDataPacket(packet, DataPacket_Kind.RELIABLE);
    this.emit(ParticipantEvent.ChatMessage, msg);
    return msg;
  }

  /**
   * Sends the given string to participants in the room via the data channel.
   * For longer messages, consider using {@link streamText} instead.
   *
   * @param text The text payload
   * @param options.topic Topic identifier used to route the stream to appropriate handlers.
   */
  async sendText(text: string, options?: SendTextOptions): Promise<TextStreamInfo> {
    return this.roomOutgoingDataStreamManager.sendText(text, options);
  }

  /**
   * Creates a new TextStreamWriter which can be used to stream text incrementally
   * to participants in the room via the data channel.
   *
   * @param options.topic Topic identifier used to route the stream to appropriate handlers.
   *
   * @internal
   * @experimental CAUTION, might get removed in a minor release
   */
  async streamText(options?: StreamTextOptions): Promise<TextStreamWriter> {
    return this.roomOutgoingDataStreamManager.streamText(options);
  }

  /** Send a File to all participants in the room via the data channel.
   * @param file The File object payload
   * @param options.topic Topic identifier used to route the stream to appropriate handlers.
   * @param options.onProgress A callback function used to monitor the upload progress percentage.
   */
  async sendFile(file: File, options?: SendFileOptions): Promise<{ id: string }> {
    return this.roomOutgoingDataStreamManager.sendFile(file, options);
  }

  /**
   * Stream bytes incrementally to participants in the room via the data channel.
   * For sending files, consider using {@link sendFile} instead.
   *
   * @param options.topic Topic identifier used to route the stream to appropriate handlers.
   */
  async streamBytes(options?: StreamBytesOptions) {
    return this.roomOutgoingDataStreamManager.streamBytes(options);
  }

  /**
   * Initiate an RPC call to a remote participant
   * @param params - Parameters for initiating the RPC call, see {@link PerformRpcParams}
   * @returns A promise that resolves with the response payload or rejects with an error.
   * @throws Error on failure. Details in `message`.
   */
  async performRpc({
    destinationIdentity,
    method,
    payload,
    responseTimeout = 15000,
  }: PerformRpcParams): Promise<string> {
    const maxRoundTripLatency = 7000;
    const minEffectiveTimeout = maxRoundTripLatency + 1000;

    return new Promise(async (resolve, reject) => {
      if (byteLength(payload) > MAX_PAYLOAD_BYTES) {
        reject(RpcError.builtIn('REQUEST_PAYLOAD_TOO_LARGE'));
        return;
      }

      if (
        this.engine.latestJoinResponse?.serverInfo?.version &&
        compareVersions(this.engine.latestJoinResponse?.serverInfo?.version, '1.8.0') < 0
      ) {
        reject(RpcError.builtIn('UNSUPPORTED_SERVER'));
        return;
      }

      const effectiveTimeout = Math.max(responseTimeout, minEffectiveTimeout);
      const id = crypto.randomUUID();
      await this.publishRpcRequest(destinationIdentity, id, method, payload, effectiveTimeout);

      const ackTimeoutId = setTimeout(() => {
        this.pendingAcks.delete(id);
        reject(RpcError.builtIn('CONNECTION_TIMEOUT'));
        this.pendingResponses.delete(id);
        clearTimeout(responseTimeoutId);
      }, maxRoundTripLatency);

      this.pendingAcks.set(id, {
        resolve: () => {
          clearTimeout(ackTimeoutId);
        },
        participantIdentity: destinationIdentity,
      });

      const responseTimeoutId = setTimeout(() => {
        this.pendingResponses.delete(id);
        reject(RpcError.builtIn('RESPONSE_TIMEOUT'));
      }, responseTimeout);

      this.pendingResponses.set(id, {
        resolve: (responsePayload: string | null, responseError: RpcError | null) => {
          clearTimeout(responseTimeoutId);
          if (this.pendingAcks.has(id)) {
            console.warn('RPC response received before ack', id);
            this.pendingAcks.delete(id);
            clearTimeout(ackTimeoutId);
          }

          if (responseError) {
            reject(responseError);
          } else {
            resolve(responsePayload ?? '');
          }
        },
        participantIdentity: destinationIdentity,
      });
    });
  }

  /**
   * @deprecated use `room.registerRpcMethod` instead
   */
  registerRpcMethod(method: string, handler: (data: RpcInvocationData) => Promise<string>) {
    if (this.rpcHandlers.has(method)) {
      this.log.warn(
        `you're overriding the RPC handler for method ${method}, in the future this will throw an error`,
      );
    }

    this.rpcHandlers.set(method, handler);
  }

  /**
   * @deprecated use `room.unregisterRpcMethod` instead
   */
  unregisterRpcMethod(method: string) {
    this.rpcHandlers.delete(method);
  }

  /**
   * Control who can subscribe to LocalParticipant's published tracks.
   *
   * By default, all participants can subscribe. This allows fine-grained control over
   * who is able to subscribe at a participant and track level.
   *
   * Note: if access is given at a track-level (i.e. both [allParticipantsAllowed] and
   * [ParticipantTrackPermission.allTracksAllowed] are false), any newer published tracks
   * will not grant permissions to any participants and will require a subsequent
   * permissions update to allow subscription.
   *
   * @param allParticipantsAllowed Allows all participants to subscribe all tracks.
   *  Takes precedence over [[participantTrackPermissions]] if set to true.
   *  By default this is set to true.
   * @param participantTrackPermissions Full list of individual permissions per
   *  participant/track. Any omitted participants will not receive any permissions.
   */
  setTrackSubscriptionPermissions(
    allParticipantsAllowed: boolean,
    participantTrackPermissions: ParticipantTrackPermission[] = [],
  ) {
    this.participantTrackPermissions = participantTrackPermissions;
    this.allParticipantsAllowedToSubscribe = allParticipantsAllowed;
    if (!this.engine.client.isDisconnected) {
      this.updateTrackSubscriptionPermissions();
    }
  }

  private handleIncomingRpcAck(requestId: string) {
    const handler = this.pendingAcks.get(requestId);
    if (handler) {
      handler.resolve();
      this.pendingAcks.delete(requestId);
    } else {
      console.error('Ack received for unexpected RPC request', requestId);
    }
  }

  private handleIncomingRpcResponse(
    requestId: string,
    payload: string | null,
    error: RpcError | null,
  ) {
    const handler = this.pendingResponses.get(requestId);
    if (handler) {
      handler.resolve(payload, error);
      this.pendingResponses.delete(requestId);
    } else {
      console.error('Response received for unexpected RPC request', requestId);
    }
  }

  /** @internal */
  private async publishRpcRequest(
    destinationIdentity: string,
    requestId: string,
    method: string,
    payload: string,
    responseTimeout: number,
  ) {
    const packet = new DataPacket({
      destinationIdentities: [destinationIdentity],
      kind: DataPacket_Kind.RELIABLE,
      value: {
        case: 'rpcRequest',
        value: new RpcRequest({
          id: requestId,
          method,
          payload,
          responseTimeoutMs: responseTimeout,
          version: 1,
        }),
      },
    });

    await this.engine.sendDataPacket(packet, DataPacket_Kind.RELIABLE);
  }

  /** @internal */
  handleParticipantDisconnected(participantIdentity: string) {
    for (const [id, { participantIdentity: pendingIdentity }] of this.pendingAcks) {
      if (pendingIdentity === participantIdentity) {
        this.pendingAcks.delete(id);
      }
    }

    for (const [id, { participantIdentity: pendingIdentity, resolve }] of this.pendingResponses) {
      if (pendingIdentity === participantIdentity) {
        resolve(null, RpcError.builtIn('RECIPIENT_DISCONNECTED'));
        this.pendingResponses.delete(id);
      }
    }
  }

  /** @internal */
  setEnabledPublishCodecs(codecs: Codec[]) {
    this.enabledPublishVideoCodecs = codecs.filter(
      (c) => c.mime.split('/')[0].toLowerCase() === 'video',
    );
  }

  /** @internal */
  updateInfo(info: ParticipantInfo): boolean {
    if (!super.updateInfo(info)) {
      return false;
    }

    // reconcile track mute status.
    // if server's track mute status doesn't match actual, we'll have to update
    // the server's copy
    info.tracks.forEach((ti) => {
      const pub = this.trackPublications.get(ti.sid);

      if (pub) {
        const mutedOnServer = pub.isMuted || (pub.track?.isUpstreamPaused ?? false);
        if (mutedOnServer !== ti.muted) {
          this.log.debug('updating server mute state after reconcile', {
            ...this.logContext,
            ...getLogContextFromTrack(pub),
            mutedOnServer,
          });
          this.engine.client.sendMuteTrack(ti.sid, mutedOnServer);
        }
      }
    });
    return true;
  }

  private updateTrackSubscriptionPermissions = () => {
    this.log.debug('updating track subscription permissions', {
      ...this.logContext,
      allParticipantsAllowed: this.allParticipantsAllowedToSubscribe,
      participantTrackPermissions: this.participantTrackPermissions,
    });
    this.engine.client.sendUpdateSubscriptionPermissions(
      this.allParticipantsAllowedToSubscribe,
      this.participantTrackPermissions.map((p) => trackPermissionToProto(p)),
    );
  };

  /** @internal */
  setActiveAgent(agent: RemoteParticipant | undefined) {
    this.firstActiveAgent = agent;
    if (agent && !this.firstActiveAgent) {
      this.firstActiveAgent = agent;
    }
    if (agent) {
      this.activeAgentFuture?.resolve?.(agent);
    } else {
      this.activeAgentFuture?.reject?.('Agent disconnected');
    }
    this.activeAgentFuture = undefined;
  }

  private waitUntilActiveAgentPresent() {
    if (this.firstActiveAgent) {
      return Promise.resolve(this.firstActiveAgent);
    }
    if (!this.activeAgentFuture) {
      this.activeAgentFuture = new Future<RemoteParticipant>();
    }
    return this.activeAgentFuture.promise;
  }

  /** @internal */
  private onTrackUnmuted = (track: LocalTrack) => {
    this.onTrackMuted(track, track.isUpstreamPaused);
  };

  // when the local track changes in mute status, we'll notify server as such
  /** @internal */
  private onTrackMuted = (track: LocalTrack, muted?: boolean) => {
    if (muted === undefined) {
      muted = true;
    }

    if (!track.sid) {
      this.log.error('could not update mute status for unpublished track', {
        ...this.logContext,
        ...getLogContextFromTrack(track),
      });
      return;
    }

    this.engine.updateMuteStatus(track.sid, muted);
  };

  private onTrackUpstreamPaused = (track: LocalTrack) => {
    this.log.debug('upstream paused', {
      ...this.logContext,
      ...getLogContextFromTrack(track),
    });
    this.onTrackMuted(track, true);
  };

  private onTrackUpstreamResumed = (track: LocalTrack) => {
    this.log.debug('upstream resumed', {
      ...this.logContext,
      ...getLogContextFromTrack(track),
    });
    this.onTrackMuted(track, track.isMuted);
  };

  private onTrackFeatureUpdate = (track: LocalAudioTrack) => {
    const pub = this.audioTrackPublications.get(track.sid!);
    if (!pub) {
      this.log.warn(
        `Could not update local audio track settings, missing publication for track ${track.sid}`,
        this.logContext,
      );
      return;
    }
    this.engine.client.sendUpdateLocalAudioTrack(pub.trackSid, pub.getTrackFeatures());
  };

  private onTrackCpuConstrained = (track: LocalVideoTrack, publication: LocalTrackPublication) => {
    this.log.debug('track cpu constrained', {
      ...this.logContext,
      ...getLogContextFromTrack(publication),
    });
    this.emit(ParticipantEvent.LocalTrackCpuConstrained, track, publication);
  };

  private handleSubscribedQualityUpdate = async (update: SubscribedQualityUpdate) => {
    if (!this.roomOptions?.dynacast) {
      return;
    }
    const pub = this.videoTrackPublications.get(update.trackSid);
    if (!pub) {
      this.log.warn('received subscribed quality update for unknown track', {
        ...this.logContext,
        trackSid: update.trackSid,
      });
      return;
    }
    if (!pub.videoTrack) {
      return;
    }
    const newCodecs = await pub.videoTrack.setPublishingCodecs(update.subscribedCodecs);
    for await (const codec of newCodecs) {
      if (isBackupCodec(codec)) {
        this.log.debug(`publish ${codec} for ${pub.videoTrack.sid}`, {
          ...this.logContext,
          ...getLogContextFromTrack(pub),
        });
        await this.publishAdditionalCodecForTrack(pub.videoTrack, codec, pub.options);
      }
    }
  };

  private handleLocalTrackUnpublished = (unpublished: TrackUnpublishedResponse) => {
    const track = this.trackPublications.get(unpublished.trackSid);
    if (!track) {
      this.log.warn('received unpublished event for unknown track', {
        ...this.logContext,
        trackSid: unpublished.trackSid,
      });
      return;
    }
    this.unpublishTrack(track.track!);
  };

  private handleTrackEnded = async (track: LocalTrack) => {
    if (
      track.source === Track.Source.ScreenShare ||
      track.source === Track.Source.ScreenShareAudio
    ) {
      this.log.debug('unpublishing local track due to TrackEnded', {
        ...this.logContext,
        ...getLogContextFromTrack(track),
      });
      this.unpublishTrack(track);
    } else if (track.isUserProvided) {
      await track.mute();
    } else if (isLocalAudioTrack(track) || isLocalVideoTrack(track)) {
      try {
        if (isWeb()) {
          try {
            const currentPermissions = await navigator?.permissions.query({
              // the permission query for camera and microphone currently not supported in Safari and Firefox
              // @ts-ignore
              name: track.source === Track.Source.Camera ? 'camera' : 'microphone',
            });
            if (currentPermissions && currentPermissions.state === 'denied') {
              this.log.warn(`user has revoked access to ${track.source}`, {
                ...this.logContext,
                ...getLogContextFromTrack(track),
              });

              // detect granted change after permissions were denied to try and resume then
              currentPermissions.onchange = () => {
                if (currentPermissions.state !== 'denied') {
                  if (!track.isMuted) {
                    track.restartTrack();
                  }
                  currentPermissions.onchange = null;
                }
              };
              throw new Error('GetUserMedia Permission denied');
            }
          } catch (e: any) {
            // permissions query fails for firefox, we continue and try to restart the track
          }
        }
        if (!track.isMuted) {
          this.log.debug('track ended, attempting to use a different device', {
            ...this.logContext,
            ...getLogContextFromTrack(track),
          });
          if (isLocalAudioTrack(track)) {
            // fall back to default device if available
            await track.restartTrack({ deviceId: 'default' });
          } else {
            await track.restartTrack();
          }
        }
      } catch (e) {
        this.log.warn(`could not restart track, muting instead`, {
          ...this.logContext,
          ...getLogContextFromTrack(track),
        });
        await track.mute();
      }
    }
  };

  private getPublicationForTrack(
    track: LocalTrack | MediaStreamTrack,
  ): LocalTrackPublication | undefined {
    let publication: LocalTrackPublication | undefined;
    this.trackPublications.forEach((pub) => {
      const localTrack = pub.track;
      if (!localTrack) {
        return;
      }

      // this looks overly complicated due to this object tree
      if (track instanceof MediaStreamTrack) {
        if (isLocalAudioTrack(localTrack) || isLocalVideoTrack(localTrack)) {
          if (localTrack.mediaStreamTrack === track) {
            publication = <LocalTrackPublication>pub;
          }
        }
      } else if (track === localTrack) {
        publication = <LocalTrackPublication>pub;
      }
    });
    return publication;
  }

  private async waitForPendingPublicationOfSource(source: Track.Source) {
    const waitForPendingTimeout = 10_000;
    const startTime = Date.now();

    while (Date.now() < startTime + waitForPendingTimeout) {
      const publishPromiseEntry = Array.from(this.pendingPublishPromises.entries()).find(
        ([pendingTrack]) => pendingTrack.source === source,
      );
      if (publishPromiseEntry) {
        return publishPromiseEntry[1];
      }
      await sleep(20);
    }
  }
}
