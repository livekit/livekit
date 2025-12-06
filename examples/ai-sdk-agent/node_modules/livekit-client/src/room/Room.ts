import { Mutex } from '@livekit/mutex';
import {
  ChatMessage as ChatMessageModel,
  ConnectionQualityUpdate,
  type DataPacket,
  DataPacket_Kind,
  DisconnectReason,
  Encryption_Type,
  JoinResponse,
  LeaveRequest,
  LeaveRequest_Action,
  MetricsBatch,
  ParticipantInfo,
  ParticipantInfo_State,
  ParticipantPermission,
  Room as RoomModel,
  ServerInfo,
  SimulateScenario,
  SipDTMF,
  SpeakerInfo,
  StreamStateUpdate,
  SubscriptionError,
  SubscriptionPermissionUpdate,
  SubscriptionResponse,
  TrackInfo,
  TrackSource,
  TrackType,
  Transcription as TranscriptionModel,
  TranscriptionSegment as TranscriptionSegmentModel,
  UserPacket,
  protoInt64,
} from '@livekit/protocol';
import { EventEmitter } from 'events';
import type TypedEmitter from 'typed-emitter';
import 'webrtc-adapter';
import { EncryptionEvent } from '../e2ee';
import { type BaseE2EEManager, E2EEManager } from '../e2ee/E2eeManager';
import log, { LoggerNames, getLogger } from '../logger';
import type {
  InternalRoomConnectOptions,
  InternalRoomOptions,
  RoomConnectOptions,
  RoomOptions,
} from '../options';
import { getBrowser } from '../utils/browserParser';
import DeviceManager from './DeviceManager';
import RTCEngine from './RTCEngine';
import { RegionUrlProvider } from './RegionUrlProvider';
import IncomingDataStreamManager from './data-stream/incoming/IncomingDataStreamManager';
import {
  type ByteStreamHandler,
  type TextStreamHandler,
} from './data-stream/incoming/StreamReader';
import OutgoingDataStreamManager from './data-stream/outgoing/OutgoingDataStreamManager';
import {
  audioDefaults,
  publishDefaults,
  roomConnectOptionDefaults,
  roomOptionDefaults,
  videoDefaults,
} from './defaults';
import { ConnectionError, ConnectionErrorReason, UnsupportedServer } from './errors';
import { EngineEvent, ParticipantEvent, RoomEvent, TrackEvent } from './events';
import LocalParticipant from './participant/LocalParticipant';
import type Participant from './participant/Participant';
import { type ConnectionQuality, ParticipantKind } from './participant/Participant';
import RemoteParticipant from './participant/RemoteParticipant';
import { MAX_PAYLOAD_BYTES, RpcError, type RpcInvocationData, byteLength } from './rpc';
import CriticalTimers from './timers';
import LocalAudioTrack from './track/LocalAudioTrack';
import type LocalTrack from './track/LocalTrack';
import LocalTrackPublication from './track/LocalTrackPublication';
import LocalVideoTrack from './track/LocalVideoTrack';
import type RemoteTrack from './track/RemoteTrack';
import RemoteTrackPublication from './track/RemoteTrackPublication';
import { Track } from './track/Track';
import type { TrackPublication } from './track/TrackPublication';
import type { TrackProcessor } from './track/processor/types';
import type { AdaptiveStreamSettings } from './track/types';
import { getNewAudioContext, kindToSource, sourceToKind } from './track/utils';
import {
  type ChatMessage,
  type SimulationOptions,
  type SimulationScenario,
  type TranscriptionSegment,
} from './types';
import {
  Future,
  createDummyVideoStreamTrack,
  extractChatMessage,
  extractTranscriptionSegments,
  getDisconnectReasonFromConnectionError,
  getEmptyAudioStreamTrack,
  isBrowserSupported,
  isCloud,
  isLocalAudioTrack,
  isLocalParticipant,
  isReactNative,
  isRemotePub,
  isSafariBased,
  isWeb,
  numberToBigInt,
  sleep,
  supportsSetSinkId,
  toHttpUrl,
  unpackStreamId,
  unwrapConstraint,
} from './utils';

export enum ConnectionState {
  Disconnected = 'disconnected',
  Connecting = 'connecting',
  Connected = 'connected',
  Reconnecting = 'reconnecting',
  SignalReconnecting = 'signalReconnecting',
}

const connectionReconcileFrequency = 4 * 1000;

/**
 * In LiveKit, a room is the logical grouping for a list of participants.
 * Participants in a room can publish tracks, and subscribe to others' tracks.
 *
 * a Room fires [[RoomEvent | RoomEvents]].
 *
 * @noInheritDoc
 */
class Room extends (EventEmitter as new () => TypedEmitter<RoomEventCallbacks>) {
  state: ConnectionState = ConnectionState.Disconnected;

  /**
   * map of identity: [[RemoteParticipant]]
   */
  remoteParticipants: Map<string, RemoteParticipant>;

  /**
   * list of participants that are actively speaking. when this changes
   * a [[RoomEvent.ActiveSpeakersChanged]] event is fired
   */
  activeSpeakers: Participant[] = [];

  /** @internal */
  engine!: RTCEngine;

  /** the current participant */
  localParticipant: LocalParticipant;

  /** options of room */
  options: InternalRoomOptions;

  /** reflects the sender encryption status of the local participant */
  isE2EEEnabled: boolean = false;

  serverInfo?: Partial<ServerInfo>;

  private roomInfo?: RoomModel;

  private sidToIdentity: Map<string, string>;

  /** connect options of room */
  private connOptions?: InternalRoomConnectOptions;

  private audioEnabled = true;

  private audioContext?: AudioContext;

  /** used for aborting pending connections to a LiveKit server */
  private abortController?: AbortController;

  /** future holding client initiated connection attempt */
  private connectFuture?: Future<void>;

  private disconnectLock: Mutex;

  private e2eeManager: BaseE2EEManager | undefined;

  private connectionReconcileInterval?: ReturnType<typeof setInterval>;

  private regionUrlProvider?: RegionUrlProvider;

  private regionUrl?: string;

  private isVideoPlaybackBlocked: boolean = false;

  private log = log;

  private bufferedEvents: Array<any> = [];

  private isResuming: boolean = false;

  /**
   * map to store first point in time when a particular transcription segment was received
   */
  private transcriptionReceivedTimes: Map<string, number>;

  private incomingDataStreamManager: IncomingDataStreamManager;

  private outgoingDataStreamManager: OutgoingDataStreamManager;

  private rpcHandlers: Map<string, (data: RpcInvocationData) => Promise<string>> = new Map();

  get hasE2EESetup(): boolean {
    return this.e2eeManager !== undefined;
  }

  /**
   * Creates a new Room, the primary construct for a LiveKit session.
   * @param options
   */
  constructor(options?: RoomOptions) {
    super();
    this.setMaxListeners(100);
    this.remoteParticipants = new Map();
    this.sidToIdentity = new Map();
    this.options = { ...roomOptionDefaults, ...options };

    this.log = getLogger(this.options.loggerName ?? LoggerNames.Room);
    this.transcriptionReceivedTimes = new Map();

    this.options.audioCaptureDefaults = {
      ...audioDefaults,
      ...options?.audioCaptureDefaults,
    };
    this.options.videoCaptureDefaults = {
      ...videoDefaults,
      ...options?.videoCaptureDefaults,
    };
    this.options.publishDefaults = {
      ...publishDefaults,
      ...options?.publishDefaults,
    };

    this.maybeCreateEngine();

    this.incomingDataStreamManager = new IncomingDataStreamManager();
    this.outgoingDataStreamManager = new OutgoingDataStreamManager(this.engine, this.log);

    this.disconnectLock = new Mutex();

    this.localParticipant = new LocalParticipant(
      '',
      '',
      this.engine,
      this.options,
      this.rpcHandlers,
      this.outgoingDataStreamManager,
    );

    if (this.options.e2ee || this.options.encryption) {
      this.setupE2EE();
    }

    this.engine.e2eeManager = this.e2eeManager;

    if (this.options.videoCaptureDefaults.deviceId) {
      this.localParticipant.activeDeviceMap.set(
        'videoinput',
        unwrapConstraint(this.options.videoCaptureDefaults.deviceId),
      );
    }
    if (this.options.audioCaptureDefaults.deviceId) {
      this.localParticipant.activeDeviceMap.set(
        'audioinput',
        unwrapConstraint(this.options.audioCaptureDefaults.deviceId),
      );
    }
    if (this.options.audioOutput?.deviceId) {
      this.switchActiveDevice(
        'audiooutput',
        unwrapConstraint(this.options.audioOutput.deviceId),
      ).catch((e) => this.log.warn(`Could not set audio output: ${e.message}`, this.logContext));
    }

    if (isWeb()) {
      const abortController = new AbortController();

      // in order to catch device changes prior to room connection we need to register the event in the constructor
      navigator.mediaDevices?.addEventListener('devicechange', this.handleDeviceChange, {
        signal: abortController.signal,
      });

      if (Room.cleanupRegistry) {
        Room.cleanupRegistry.register(this, () => {
          abortController.abort();
        });
      }
    }
  }

  registerTextStreamHandler(topic: string, callback: TextStreamHandler) {
    return this.incomingDataStreamManager.registerTextStreamHandler(topic, callback);
  }

  unregisterTextStreamHandler(topic: string) {
    return this.incomingDataStreamManager.unregisterTextStreamHandler(topic);
  }

  registerByteStreamHandler(topic: string, callback: ByteStreamHandler) {
    return this.incomingDataStreamManager.registerByteStreamHandler(topic, callback);
  }

  unregisterByteStreamHandler(topic: string) {
    return this.incomingDataStreamManager.unregisterByteStreamHandler(topic);
  }

  /**
   * Establishes the participant as a receiver for calls of the specified RPC method.
   *
   * @param method - The name of the indicated RPC method
   * @param handler - Will be invoked when an RPC request for this method is received
   * @returns A promise that resolves when the method is successfully registered
   * @throws {Error} If a handler for this method is already registered (must call unregisterRpcMethod first)
   *
   * @example
   * ```typescript
   * room.localParticipant?.registerRpcMethod(
   *   'greet',
   *   async (data: RpcInvocationData) => {
   *     console.log(`Received greeting from ${data.callerIdentity}: ${data.payload}`);
   *     return `Hello, ${data.callerIdentity}!`;
   *   }
   * );
   * ```
   *
   * The handler should return a Promise that resolves to a string.
   * If unable to respond within `responseTimeout`, the request will result in an error on the caller's side.
   *
   * You may throw errors of type `RpcError` with a string `message` in the handler,
   * and they will be received on the caller's side with the message intact.
   * Other errors thrown in your handler will not be transmitted as-is, and will instead arrive to the caller as `1500` ("Application Error").
   */
  registerRpcMethod(method: string, handler: (data: RpcInvocationData) => Promise<string>) {
    if (this.rpcHandlers.has(method)) {
      throw Error(
        `RPC handler already registered for method ${method}, unregisterRpcMethod before trying to register again`,
      );
    }
    this.rpcHandlers.set(method, handler);
  }

  /**
   * Unregisters a previously registered RPC method.
   *
   * @param method - The name of the RPC method to unregister
   */
  unregisterRpcMethod(method: string) {
    this.rpcHandlers.delete(method);
  }

  /**
   * @experimental
   */
  async setE2EEEnabled(enabled: boolean) {
    if (this.e2eeManager) {
      await Promise.all([this.localParticipant.setE2EEEnabled(enabled)]);
      if (this.localParticipant.identity !== '') {
        this.e2eeManager.setParticipantCryptorEnabled(enabled, this.localParticipant.identity);
      }
    } else {
      throw Error('e2ee not configured, please set e2ee settings within the room options');
    }
  }

  private setupE2EE() {
    // when encryption is enabled via `options.encryption`, we enable data channel encryption

    const dcEncryptionEnabled = !!this.options.encryption;
    const e2eeOptions = this.options.encryption || this.options.e2ee;

    if (e2eeOptions) {
      if ('e2eeManager' in e2eeOptions) {
        this.e2eeManager = e2eeOptions.e2eeManager;
        this.e2eeManager.isDataChannelEncryptionEnabled = dcEncryptionEnabled;
      } else {
        this.e2eeManager = new E2EEManager(e2eeOptions, dcEncryptionEnabled);
      }
      this.e2eeManager.on(
        EncryptionEvent.ParticipantEncryptionStatusChanged,
        (enabled, participant) => {
          if (isLocalParticipant(participant)) {
            this.isE2EEEnabled = enabled;
          }
          this.emit(RoomEvent.ParticipantEncryptionStatusChanged, enabled, participant);
        },
      );
      this.e2eeManager.on(EncryptionEvent.EncryptionError, (error) =>
        this.emit(RoomEvent.EncryptionError, error),
      );
      this.e2eeManager?.setup(this);
    }
  }

  private get logContext() {
    return {
      room: this.name,
      roomID: this.roomInfo?.sid,
      participant: this.localParticipant.identity,
      pID: this.localParticipant.sid,
    };
  }

  /**
   * if the current room has a participant with `recorder: true` in its JWT grant
   **/
  get isRecording(): boolean {
    return this.roomInfo?.activeRecording ?? false;
  }

  /**
   * server assigned unique room id.
   * returns once a sid has been issued by the server.
   */
  async getSid(): Promise<string> {
    if (this.state === ConnectionState.Disconnected) {
      return '';
    }
    if (this.roomInfo && this.roomInfo.sid !== '') {
      return this.roomInfo.sid;
    }
    return new Promise((resolve, reject) => {
      const handleRoomUpdate = (roomInfo: RoomModel) => {
        if (roomInfo.sid !== '') {
          this.engine.off(EngineEvent.RoomUpdate, handleRoomUpdate);
          resolve(roomInfo.sid);
        }
      };
      this.engine.on(EngineEvent.RoomUpdate, handleRoomUpdate);
      this.once(RoomEvent.Disconnected, () => {
        this.engine.off(EngineEvent.RoomUpdate, handleRoomUpdate);
        reject('Room disconnected before room server id was available');
      });
    });
  }

  /** user assigned name, derived from JWT token */
  get name(): string {
    return this.roomInfo?.name ?? '';
  }

  /** room metadata */
  get metadata(): string | undefined {
    return this.roomInfo?.metadata;
  }

  get numParticipants(): number {
    return this.roomInfo?.numParticipants ?? 0;
  }

  get numPublishers(): number {
    return this.roomInfo?.numPublishers ?? 0;
  }

  private maybeCreateEngine() {
    if (this.engine && !this.engine.isClosed) {
      return;
    }

    this.engine = new RTCEngine(this.options);
    this.engine.e2eeManager = this.e2eeManager;

    this.engine
      .on(EngineEvent.ParticipantUpdate, this.handleParticipantUpdates)
      .on(EngineEvent.RoomUpdate, this.handleRoomUpdate)
      .on(EngineEvent.SpeakersChanged, this.handleSpeakersChanged)
      .on(EngineEvent.StreamStateChanged, this.handleStreamStateUpdate)
      .on(EngineEvent.ConnectionQualityUpdate, this.handleConnectionQualityUpdate)
      .on(EngineEvent.SubscriptionError, this.handleSubscriptionError)
      .on(EngineEvent.SubscriptionPermissionUpdate, this.handleSubscriptionPermissionUpdate)
      .on(
        EngineEvent.MediaTrackAdded,
        (mediaTrack: MediaStreamTrack, stream: MediaStream, receiver: RTCRtpReceiver) => {
          this.onTrackAdded(mediaTrack, stream, receiver);
        },
      )
      .on(EngineEvent.Disconnected, (reason?: DisconnectReason) => {
        this.handleDisconnect(this.options.stopLocalTrackOnUnpublish, reason);
      })
      .on(EngineEvent.ActiveSpeakersUpdate, this.handleActiveSpeakersUpdate)
      .on(EngineEvent.DataPacketReceived, this.handleDataPacket)
      .on(EngineEvent.Resuming, () => {
        this.clearConnectionReconcile();
        this.isResuming = true;
        this.log.info('Resuming signal connection', this.logContext);
        if (this.setAndEmitConnectionState(ConnectionState.SignalReconnecting)) {
          this.emit(RoomEvent.SignalReconnecting);
        }
      })
      .on(EngineEvent.Resumed, () => {
        this.registerConnectionReconcile();
        this.isResuming = false;
        this.log.info('Resumed signal connection', this.logContext);
        this.updateSubscriptions();
        this.emitBufferedEvents();
        if (this.setAndEmitConnectionState(ConnectionState.Connected)) {
          this.emit(RoomEvent.Reconnected);
        }
      })
      .on(EngineEvent.SignalResumed, () => {
        this.bufferedEvents = [];
        if (this.state === ConnectionState.Reconnecting || this.isResuming) {
          this.sendSyncState();
        }
      })
      .on(EngineEvent.Restarting, this.handleRestarting)
      .on(EngineEvent.SignalRestarted, this.handleSignalRestarted)
      .on(EngineEvent.Offline, () => {
        if (this.setAndEmitConnectionState(ConnectionState.Reconnecting)) {
          this.emit(RoomEvent.Reconnecting);
        }
      })
      .on(EngineEvent.DCBufferStatusChanged, (status, kind) => {
        this.emit(RoomEvent.DCBufferStatusChanged, status, kind);
      })
      .on(EngineEvent.LocalTrackSubscribed, (subscribedSid) => {
        const trackPublication = this.localParticipant
          .getTrackPublications()
          .find(({ trackSid }) => trackSid === subscribedSid) as LocalTrackPublication | undefined;
        if (!trackPublication) {
          this.log.warn(
            'could not find local track subscription for subscribed event',
            this.logContext,
          );
          return;
        }
        this.localParticipant.emit(ParticipantEvent.LocalTrackSubscribed, trackPublication);
        this.emitWhenConnected(
          RoomEvent.LocalTrackSubscribed,
          trackPublication,
          this.localParticipant,
        );
      })
      .on(EngineEvent.RoomMoved, (roomMoved) => {
        this.log.debug('room moved', roomMoved);

        if (roomMoved.room) {
          this.handleRoomUpdate(roomMoved.room);
        }

        this.remoteParticipants.forEach((participant, identity) => {
          this.handleParticipantDisconnected(identity, participant);
        });

        this.emit(RoomEvent.Moved, roomMoved.room!.name);

        if (roomMoved.participant) {
          this.handleParticipantUpdates([roomMoved.participant, ...roomMoved.otherParticipants]);
        } else {
          this.handleParticipantUpdates(roomMoved.otherParticipants);
        }
      });

    if (this.localParticipant) {
      this.localParticipant.setupEngine(this.engine);
    }
    if (this.e2eeManager) {
      this.e2eeManager.setupEngine(this.engine);
    }
    if (this.outgoingDataStreamManager) {
      this.outgoingDataStreamManager.setupEngine(this.engine);
    }
  }

  /**
   * getLocalDevices abstracts navigator.mediaDevices.enumerateDevices.
   * In particular, it requests device permissions by default if needed
   * and makes sure the returned device does not consist of dummy devices
   * @param kind
   * @returns a list of available local devices
   */
  static getLocalDevices(
    kind?: MediaDeviceKind,
    requestPermissions: boolean = true,
  ): Promise<MediaDeviceInfo[]> {
    return DeviceManager.getInstance().getDevices(kind, requestPermissions);
  }

  static cleanupRegistry =
    typeof FinalizationRegistry !== 'undefined' &&
    new FinalizationRegistry((cleanup: () => void) => {
      cleanup();
    });

  /**
   * prepareConnection should be called as soon as the page is loaded, in order
   * to speed up the connection attempt. This function will
   * - perform DNS resolution and pre-warm the DNS cache
   * - establish TLS connection and cache TLS keys
   *
   * With LiveKit Cloud, it will also determine the best edge data center for
   * the current client to connect to if a token is provided.
   */
  async prepareConnection(url: string, token?: string) {
    if (this.state !== ConnectionState.Disconnected) {
      return;
    }
    this.log.debug(`prepareConnection to ${url}`, this.logContext);
    try {
      if (isCloud(new URL(url)) && token) {
        this.regionUrlProvider = new RegionUrlProvider(url, token);
        const regionUrl = await this.regionUrlProvider.getNextBestRegionUrl();
        // we will not replace the regionUrl if an attempt had already started
        // to avoid overriding regionUrl after a new connection attempt had started
        if (regionUrl && this.state === ConnectionState.Disconnected) {
          this.regionUrl = regionUrl;
          await fetch(toHttpUrl(regionUrl), { method: 'HEAD' });
          this.log.debug(`prepared connection to ${regionUrl}`, this.logContext);
        }
      } else {
        await fetch(toHttpUrl(url), { method: 'HEAD' });
      }
    } catch (e) {
      this.log.warn('could not prepare connection', { ...this.logContext, error: e });
    }
  }

  connect = async (url: string, token: string, opts?: RoomConnectOptions): Promise<void> => {
    if (!isBrowserSupported()) {
      if (isReactNative()) {
        throw Error("WebRTC isn't detected, have you called registerGlobals?");
      } else {
        throw Error(
          "LiveKit doesn't seem to be supported on this browser. Try to update your browser and make sure no browser extensions are disabling webRTC.",
        );
      }
    }

    // In case a disconnect called happened right before the connect call, make sure the disconnect is completed first by awaiting its lock
    const unlockDisconnect = await this.disconnectLock.lock();

    if (this.state === ConnectionState.Connected) {
      // when the state is reconnecting or connected, this function returns immediately
      this.log.info(`already connected to room ${this.name}`, this.logContext);
      unlockDisconnect();
      return Promise.resolve();
    }

    if (this.connectFuture) {
      unlockDisconnect();
      return this.connectFuture.promise;
    }

    this.setAndEmitConnectionState(ConnectionState.Connecting);
    if (this.regionUrlProvider?.getServerUrl().toString() !== url) {
      this.regionUrl = undefined;
      this.regionUrlProvider = undefined;
    }
    if (isCloud(new URL(url))) {
      if (this.regionUrlProvider === undefined) {
        this.regionUrlProvider = new RegionUrlProvider(url, token);
      } else {
        this.regionUrlProvider.updateToken(token);
      }
      // trigger the first fetch without waiting for a response
      // if initial connection fails, this will speed up picking regional url
      // on subsequent runs
      this.regionUrlProvider
        .fetchRegionSettings()
        .then((settings) => {
          this.regionUrlProvider?.setServerReportedRegions(settings);
        })
        .catch((e) => {
          this.log.warn('could not fetch region settings', { ...this.logContext, error: e });
        });
    }

    const connectFn = async (
      resolve: () => void,
      reject: (reason: any) => void,
      regionUrl?: string,
    ) => {
      if (this.abortController) {
        this.abortController.abort();
      }

      // explicit creation as local var needed to satisfy TS compiler when passing it to `attemptConnection` further down
      const abortController = new AbortController();
      this.abortController = abortController;

      // at this point the intention to connect has been signalled so we can allow cancelling of the connection via disconnect() again
      unlockDisconnect?.();

      try {
        await this.attemptConnection(regionUrl ?? url, token, opts, abortController);
        this.abortController = undefined;
        resolve();
      } catch (e) {
        if (
          this.regionUrlProvider &&
          e instanceof ConnectionError &&
          e.reason !== ConnectionErrorReason.Cancelled &&
          e.reason !== ConnectionErrorReason.NotAllowed
        ) {
          let nextUrl: string | null = null;
          try {
            nextUrl = await this.regionUrlProvider.getNextBestRegionUrl(
              this.abortController?.signal,
            );
          } catch (error) {
            if (
              error instanceof ConnectionError &&
              (error.status === 401 || error.reason === ConnectionErrorReason.Cancelled)
            ) {
              this.handleDisconnect(this.options.stopLocalTrackOnUnpublish);
              reject(error);
              return;
            }
          }
          if (nextUrl && !this.abortController?.signal.aborted) {
            this.log.info(
              `Initial connection failed with ConnectionError: ${e.message}. Retrying with another region: ${nextUrl}`,
              this.logContext,
            );
            this.recreateEngine();
            await connectFn(resolve, reject, nextUrl);
          } else {
            this.handleDisconnect(
              this.options.stopLocalTrackOnUnpublish,
              getDisconnectReasonFromConnectionError(e),
            );
            reject(e);
          }
        } else {
          let disconnectReason = DisconnectReason.UNKNOWN_REASON;
          if (e instanceof ConnectionError) {
            disconnectReason = getDisconnectReasonFromConnectionError(e);
          }
          this.handleDisconnect(this.options.stopLocalTrackOnUnpublish, disconnectReason);
          reject(e);
        }
      }
    };

    const regionUrl = this.regionUrl;
    this.regionUrl = undefined;
    this.connectFuture = new Future(
      (resolve, reject) => {
        connectFn(resolve, reject, regionUrl);
      },
      () => {
        this.clearConnectionFutures();
      },
    );

    return this.connectFuture.promise;
  };

  private connectSignal = async (
    url: string,
    token: string,
    engine: RTCEngine,
    connectOptions: InternalRoomConnectOptions,
    roomOptions: InternalRoomOptions,
    abortController: AbortController,
  ): Promise<JoinResponse> => {
    const joinResponse = await engine.join(
      url,
      token,
      {
        autoSubscribe: connectOptions.autoSubscribe,
        adaptiveStream:
          typeof roomOptions.adaptiveStream === 'object' ? true : roomOptions.adaptiveStream,
        maxRetries: connectOptions.maxRetries,
        e2eeEnabled: !!this.e2eeManager,
        websocketTimeout: connectOptions.websocketTimeout,
        singlePeerConnection: roomOptions.singlePeerConnection,
      },
      abortController.signal,
    );

    let serverInfo: Partial<ServerInfo> | undefined = joinResponse.serverInfo;
    if (!serverInfo) {
      serverInfo = { version: joinResponse.serverVersion, region: joinResponse.serverRegion };
    }
    this.serverInfo = serverInfo;

    this.log.debug(
      `connected to Livekit Server ${Object.entries(serverInfo)
        .map(([key, value]) => `${key}: ${value}`)
        .join(', ')}`,
      {
        room: joinResponse.room?.name,
        roomSid: joinResponse.room?.sid,
        identity: joinResponse.participant?.identity,
      },
    );

    if (!serverInfo.version) {
      throw new UnsupportedServer('unknown server version');
    }

    if (serverInfo.version === '0.15.1' && this.options.dynacast) {
      this.log.debug('disabling dynacast due to server version', this.logContext);
      // dynacast has a bug in 0.15.1, so we cannot use it then
      roomOptions.dynacast = false;
    }

    return joinResponse;
  };

  private applyJoinResponse = (joinResponse: JoinResponse) => {
    const pi = joinResponse.participant!;

    this.localParticipant.sid = pi.sid;
    this.localParticipant.identity = pi.identity;
    this.localParticipant.setEnabledPublishCodecs(joinResponse.enabledPublishCodecs);

    if (this.e2eeManager) {
      try {
        this.e2eeManager.setSifTrailer(joinResponse.sifTrailer);
      } catch (e: any) {
        this.log.error(e instanceof Error ? e.message : 'Could not set SifTrailer', {
          ...this.logContext,
          error: e,
        });
      }
    }

    // populate remote participants, these should not trigger new events
    this.handleParticipantUpdates([pi, ...joinResponse.otherParticipants]);

    if (joinResponse.room) {
      this.handleRoomUpdate(joinResponse.room);
    }
  };

  private attemptConnection = async (
    url: string,
    token: string,
    opts: RoomConnectOptions | undefined,
    abortController: AbortController,
  ) => {
    if (
      this.state === ConnectionState.Reconnecting ||
      this.isResuming ||
      this.engine?.pendingReconnect
    ) {
      this.log.info('Reconnection attempt replaced by new connection attempt', this.logContext);
      // make sure we close and recreate the existing engine in order to get rid of any potentially ongoing reconnection attempts
      this.recreateEngine();
    } else {
      // create engine if previously disconnected
      this.maybeCreateEngine();
    }
    if (this.regionUrlProvider?.isCloud()) {
      this.engine.setRegionUrlProvider(this.regionUrlProvider);
    }

    this.acquireAudioContext();

    this.connOptions = { ...roomConnectOptionDefaults, ...opts } as InternalRoomConnectOptions;

    if (this.connOptions.rtcConfig) {
      this.engine.rtcConfig = this.connOptions.rtcConfig;
    }
    if (this.connOptions.peerConnectionTimeout) {
      this.engine.peerConnectionTimeout = this.connOptions.peerConnectionTimeout;
    }

    try {
      const joinResponse = await this.connectSignal(
        url,
        token,
        this.engine,
        this.connOptions,
        this.options,
        abortController,
      );

      this.applyJoinResponse(joinResponse);
      // forward metadata changed for the local participant
      this.setupLocalParticipantEvents();
      this.emit(RoomEvent.SignalConnected);
    } catch (err) {
      await this.engine.close();
      this.recreateEngine();
      const resultingError = new ConnectionError(
        `could not establish signal connection`,
        ConnectionErrorReason.ServerUnreachable,
      );
      if (err instanceof Error) {
        resultingError.message = `${resultingError.message}: ${err.message}`;
      }
      if (err instanceof ConnectionError) {
        resultingError.reason = err.reason;
        resultingError.status = err.status;
      }
      this.log.debug(`error trying to establish signal connection`, {
        ...this.logContext,
        error: err,
      });
      throw resultingError;
    }

    if (abortController.signal.aborted) {
      await this.engine.close();
      this.recreateEngine();
      throw new ConnectionError(`Connection attempt aborted`, ConnectionErrorReason.Cancelled);
    }

    try {
      await this.engine.waitForPCInitialConnection(
        this.connOptions.peerConnectionTimeout,
        abortController,
      );
    } catch (e) {
      await this.engine.close();
      this.recreateEngine();
      throw e;
    }

    // also hook unload event
    if (isWeb() && this.options.disconnectOnPageLeave) {
      // capturing both 'pagehide' and 'beforeunload' to capture broadest set of browser behaviors
      window.addEventListener('pagehide', this.onPageLeave);
      window.addEventListener('beforeunload', this.onPageLeave);
    }
    if (isWeb()) {
      document.addEventListener('freeze', this.onPageLeave);
    }
    this.setAndEmitConnectionState(ConnectionState.Connected);
    this.emit(RoomEvent.Connected);
    this.registerConnectionReconcile();
  };

  /**
   * disconnects the room, emits [[RoomEvent.Disconnected]]
   */
  disconnect = async (stopTracks = true) => {
    const unlock = await this.disconnectLock.lock();
    try {
      if (this.state === ConnectionState.Disconnected) {
        this.log.debug('already disconnected', this.logContext);
        return;
      }
      this.log.info('disconnect from room', {
        ...this.logContext,
      });
      if (
        this.state === ConnectionState.Connecting ||
        this.state === ConnectionState.Reconnecting ||
        this.isResuming
      ) {
        // try aborting pending connection attempt
        const msg = 'Abort connection attempt due to user initiated disconnect';
        this.log.warn(msg, this.logContext);
        this.abortController?.abort(msg);
        // in case the abort controller didn't manage to cancel the connection attempt, reject the connect promise explicitly
        this.connectFuture?.reject?.(
          new ConnectionError('Client initiated disconnect', ConnectionErrorReason.Cancelled),
        );
        this.connectFuture = undefined;
      }
      // send leave
      if (!this.engine?.client.isDisconnected) {
        await this.engine.client.sendLeave();
      }
      // close engine (also closes client)
      if (this.engine) {
        await this.engine.close();
      }
      this.handleDisconnect(stopTracks, DisconnectReason.CLIENT_INITIATED);
      /* @ts-ignore */
      this.engine = undefined;
    } finally {
      unlock();
    }
  };

  /**
   * retrieves a participant by identity
   * @param identity
   * @returns
   */
  getParticipantByIdentity(identity: string): Participant | undefined {
    if (this.localParticipant.identity === identity) {
      return this.localParticipant;
    }
    return this.remoteParticipants.get(identity);
  }

  private clearConnectionFutures() {
    this.connectFuture = undefined;
  }

  /**
   * @internal for testing
   */
  async simulateScenario(scenario: SimulationScenario, arg?: any) {
    let postAction = async () => {};
    let req: SimulateScenario | undefined;
    switch (scenario) {
      case 'signal-reconnect':
        // @ts-expect-error function is private
        await this.engine.client.handleOnClose('simulate disconnect');
        break;
      case 'speaker':
        req = new SimulateScenario({
          scenario: {
            case: 'speakerUpdate',
            value: 3,
          },
        });
        break;
      case 'node-failure':
        req = new SimulateScenario({
          scenario: {
            case: 'nodeFailure',
            value: true,
          },
        });
        break;
      case 'server-leave':
        req = new SimulateScenario({
          scenario: {
            case: 'serverLeave',
            value: true,
          },
        });
        break;
      case 'migration':
        req = new SimulateScenario({
          scenario: {
            case: 'migration',
            value: true,
          },
        });
        break;
      case 'resume-reconnect':
        this.engine.failNext();
        // @ts-expect-error function is private
        await this.engine.client.handleOnClose('simulate resume-disconnect');
        break;
      case 'disconnect-signal-on-resume':
        postAction = async () => {
          // @ts-expect-error function is private
          await this.engine.client.handleOnClose('simulate resume-disconnect');
        };
        req = new SimulateScenario({
          scenario: {
            case: 'disconnectSignalOnResume',
            value: true,
          },
        });
        break;
      case 'disconnect-signal-on-resume-no-messages':
        postAction = async () => {
          // @ts-expect-error function is private
          await this.engine.client.handleOnClose('simulate resume-disconnect');
        };
        req = new SimulateScenario({
          scenario: {
            case: 'disconnectSignalOnResumeNoMessages',
            value: true,
          },
        });
        break;
      case 'full-reconnect':
        this.engine.fullReconnectOnNext = true;
        // @ts-expect-error function is private
        await this.engine.client.handleOnClose('simulate full-reconnect');
        break;
      case 'force-tcp':
      case 'force-tls':
        req = new SimulateScenario({
          scenario: {
            case: 'switchCandidateProtocol',
            value: scenario === 'force-tls' ? 2 : 1,
          },
        });
        postAction = async () => {
          const onLeave = this.engine.client.onLeave;
          if (onLeave) {
            onLeave(
              new LeaveRequest({
                reason: DisconnectReason.CLIENT_INITIATED,
                action: LeaveRequest_Action.RECONNECT,
              }),
            );
          }
        };
        break;
      case 'subscriber-bandwidth':
        if (arg === undefined || typeof arg !== 'number') {
          throw new Error('subscriber-bandwidth requires a number as argument');
        }
        req = new SimulateScenario({
          scenario: {
            case: 'subscriberBandwidth',
            value: numberToBigInt(arg),
          },
        });
        break;
      case 'leave-full-reconnect':
        req = new SimulateScenario({
          scenario: {
            case: 'leaveRequestFullReconnect',
            value: true,
          },
        });
      default:
    }
    if (req) {
      await this.engine.client.sendSimulateScenario(req);
      await postAction();
    }
  }

  private onPageLeave = async () => {
    this.log.info('Page leave detected, disconnecting', this.logContext);
    await this.disconnect();
  };

  /**
   * Browsers have different policies regarding audio playback. Most requiring
   * some form of user interaction (click/tap/etc).
   * In those cases, audio will be silent until a click/tap triggering one of the following
   * - `startAudio`
   * - `getUserMedia`
   */
  startAudio = async () => {
    const elements: Array<HTMLMediaElement> = [];
    const browser = getBrowser();
    if (browser && browser.os === 'iOS') {
      /**
       * iOS blocks audio element playback if
       * - user is not publishing audio themselves and
       * - no other audio source is playing
       *
       * as a workaround, we create an audio element with an empty track, so that
       * silent audio is always playing
       */
      const audioId = 'livekit-dummy-audio-el';
      let dummyAudioEl = document.getElementById(audioId) as HTMLAudioElement | null;
      if (!dummyAudioEl) {
        dummyAudioEl = document.createElement('audio');
        dummyAudioEl.id = audioId;
        dummyAudioEl.autoplay = true;
        dummyAudioEl.hidden = true;
        const track = getEmptyAudioStreamTrack();
        track.enabled = true;
        const stream = new MediaStream([track]);
        dummyAudioEl.srcObject = stream;
        document.addEventListener('visibilitychange', () => {
          if (!dummyAudioEl) {
            return;
          }
          // set the srcObject to null on page hide in order to prevent lock screen controls to show up for it
          dummyAudioEl.srcObject = document.hidden ? null : stream;
          if (!document.hidden) {
            this.log.debug(
              'page visible again, triggering startAudio to resume playback and update playback status',
              this.logContext,
            );
            this.startAudio();
          }
        });
        document.body.append(dummyAudioEl);
        this.once(RoomEvent.Disconnected, () => {
          dummyAudioEl?.remove();
          dummyAudioEl = null;
        });
      }
      elements.push(dummyAudioEl);
    }

    this.remoteParticipants.forEach((p) => {
      p.audioTrackPublications.forEach((t) => {
        if (t.track) {
          t.track.attachedElements.forEach((e) => {
            elements.push(e);
          });
        }
      });
    });

    try {
      await Promise.all([
        this.acquireAudioContext(),
        ...elements.map((e) => {
          e.muted = false;
          return e.play();
        }),
      ]);
      this.handleAudioPlaybackStarted();
    } catch (err) {
      this.handleAudioPlaybackFailed(err);
      throw err;
    }
  };

  startVideo = async () => {
    const elements: HTMLMediaElement[] = [];
    for (const p of this.remoteParticipants.values()) {
      p.videoTrackPublications.forEach((tr) => {
        tr.track?.attachedElements.forEach((el) => {
          if (!elements.includes(el)) {
            elements.push(el);
          }
        });
      });
    }
    await Promise.all(elements.map((el) => el.play()))
      .then(() => {
        this.handleVideoPlaybackStarted();
      })
      .catch((e) => {
        if (e.name === 'NotAllowedError') {
          this.handleVideoPlaybackFailed();
        } else {
          this.log.warn(
            'Resuming video playback failed, make sure you call `startVideo` directly in a user gesture handler',
            this.logContext,
          );
        }
      });
  };

  /**
   * Returns true if audio playback is enabled
   */
  get canPlaybackAudio(): boolean {
    return this.audioEnabled;
  }

  /**
   * Returns true if video playback is enabled
   */
  get canPlaybackVideo(): boolean {
    return !this.isVideoPlaybackBlocked;
  }

  getActiveDevice(kind: MediaDeviceKind): string | undefined {
    return this.localParticipant.activeDeviceMap.get(kind);
  }

  /**
   * Switches all active devices used in this room to the given device.
   *
   * Note: setting AudioOutput is not supported on some browsers. See [setSinkId](https://developer.mozilla.org/en-US/docs/Web/API/HTMLMediaElement/setSinkId#browser_compatibility)
   *
   * @param kind use `videoinput` for camera track,
   *  `audioinput` for microphone track,
   *  `audiooutput` to set speaker for all incoming audio tracks
   * @param deviceId
   */
  async switchActiveDevice(kind: MediaDeviceKind, deviceId: string, exact: boolean = true) {
    let success = true;
    let shouldTriggerImmediateDeviceChange = false;
    const deviceConstraint = exact ? { exact: deviceId } : deviceId;
    if (kind === 'audioinput') {
      shouldTriggerImmediateDeviceChange = this.localParticipant.audioTrackPublications.size === 0;
      const prevDeviceId =
        this.getActiveDevice(kind) ?? this.options.audioCaptureDefaults!.deviceId;
      this.options.audioCaptureDefaults!.deviceId = deviceConstraint;
      const tracks = Array.from(this.localParticipant.audioTrackPublications.values()).filter(
        (track) => track.source === Track.Source.Microphone,
      );
      try {
        success = (
          await Promise.all(tracks.map((t) => t.audioTrack?.setDeviceId(deviceConstraint)))
        ).every((val) => val === true);
      } catch (e) {
        this.options.audioCaptureDefaults!.deviceId = prevDeviceId;
        throw e;
      }
      const isMuted = tracks.some((t) => t.track?.isMuted ?? false);
      if (success && isMuted) shouldTriggerImmediateDeviceChange = true;
    } else if (kind === 'videoinput') {
      shouldTriggerImmediateDeviceChange = this.localParticipant.videoTrackPublications.size === 0;
      const prevDeviceId =
        this.getActiveDevice(kind) ?? this.options.videoCaptureDefaults!.deviceId;
      this.options.videoCaptureDefaults!.deviceId = deviceConstraint;
      const tracks = Array.from(this.localParticipant.videoTrackPublications.values()).filter(
        (track) => track.source === Track.Source.Camera,
      );
      try {
        success = (
          await Promise.all(tracks.map((t) => t.videoTrack?.setDeviceId(deviceConstraint)))
        ).every((val) => val === true);
      } catch (e) {
        this.options.videoCaptureDefaults!.deviceId = prevDeviceId;
        throw e;
      }
      const isMuted = tracks.some((t) => t.track?.isMuted ?? false);
      if (success && isMuted) shouldTriggerImmediateDeviceChange = true;
    } else if (kind === 'audiooutput') {
      shouldTriggerImmediateDeviceChange = true;
      if (
        (!supportsSetSinkId() && !this.options.webAudioMix) ||
        (this.options.webAudioMix && this.audioContext && !('setSinkId' in this.audioContext))
      ) {
        throw new Error('cannot switch audio output, the current browser does not support it');
      }
      if (this.options.webAudioMix) {
        // setting `default` for web audio output doesn't work, so we need to normalize the id before
        deviceId =
          (await DeviceManager.getInstance().normalizeDeviceId('audiooutput', deviceId)) ?? '';
      }
      this.options.audioOutput ??= {};
      const prevDeviceId = this.getActiveDevice(kind) ?? this.options.audioOutput.deviceId;
      this.options.audioOutput.deviceId = deviceId;

      try {
        if (this.options.webAudioMix) {
          // @ts-expect-error setSinkId is not yet in the typescript type of AudioContext
          this.audioContext?.setSinkId(deviceId);
        }

        // also set audio output on all audio elements, even if webAudioMix is enabled in order to workaround echo cancellation not working on chrome with non-default output devices
        // see https://issues.chromium.org/issues/40252911#comment7
        await Promise.all(
          Array.from(this.remoteParticipants.values()).map((p) => p.setAudioOutput({ deviceId })),
        );
      } catch (e) {
        this.options.audioOutput.deviceId = prevDeviceId;
        throw e;
      }
    }

    if (shouldTriggerImmediateDeviceChange) {
      this.localParticipant.activeDeviceMap.set(kind, deviceId);
      this.emit(RoomEvent.ActiveDeviceChanged, kind, deviceId);
    }

    return success;
  }

  private setupLocalParticipantEvents() {
    this.localParticipant
      .on(ParticipantEvent.ParticipantMetadataChanged, this.onLocalParticipantMetadataChanged)
      .on(ParticipantEvent.ParticipantNameChanged, this.onLocalParticipantNameChanged)
      .on(ParticipantEvent.AttributesChanged, this.onLocalAttributesChanged)
      .on(ParticipantEvent.TrackMuted, this.onLocalTrackMuted)
      .on(ParticipantEvent.TrackUnmuted, this.onLocalTrackUnmuted)
      .on(ParticipantEvent.LocalTrackPublished, this.onLocalTrackPublished)
      .on(ParticipantEvent.LocalTrackUnpublished, this.onLocalTrackUnpublished)
      .on(ParticipantEvent.ConnectionQualityChanged, this.onLocalConnectionQualityChanged)
      .on(ParticipantEvent.MediaDevicesError, this.onMediaDevicesError)
      .on(ParticipantEvent.AudioStreamAcquired, this.startAudio)
      .on(ParticipantEvent.ChatMessage, this.onLocalChatMessageSent)
      .on(
        ParticipantEvent.ParticipantPermissionsChanged,
        this.onLocalParticipantPermissionsChanged,
      );
  }

  private recreateEngine() {
    this.engine?.close();
    /* @ts-ignore */
    this.engine = undefined;
    this.isResuming = false;

    // clear out existing remote participants, since they may have attached
    // the old engine
    this.remoteParticipants.clear();
    this.sidToIdentity.clear();
    this.bufferedEvents = [];
    this.maybeCreateEngine();
  }

  private onTrackAdded(
    mediaTrack: MediaStreamTrack,
    stream: MediaStream,
    receiver: RTCRtpReceiver,
  ) {
    // don't fire onSubscribed when connecting
    // WebRTC fires onTrack as soon as setRemoteDescription is called on the offer
    // at that time, ICE connectivity has not been established so the track is not
    // technically subscribed.
    // We'll defer these events until when the room is connected or eventually disconnected.
    if (this.state === ConnectionState.Connecting || this.state === ConnectionState.Reconnecting) {
      const reconnectedHandler = () => {
        this.log.debug('deferring on track for later', {
          mediaTrackId: mediaTrack.id,
          mediaStreamId: stream.id,
          tracksInStream: stream.getTracks().map((track) => track.id),
        });
        this.onTrackAdded(mediaTrack, stream, receiver);
        cleanup();
      };
      const cleanup = () => {
        this.off(RoomEvent.Reconnected, reconnectedHandler);
        this.off(RoomEvent.Connected, reconnectedHandler);
        this.off(RoomEvent.Disconnected, cleanup);
      };
      this.once(RoomEvent.Reconnected, reconnectedHandler);
      this.once(RoomEvent.Connected, reconnectedHandler);
      this.once(RoomEvent.Disconnected, cleanup);
      return;
    }
    if (this.state === ConnectionState.Disconnected) {
      this.log.warn('skipping incoming track after Room disconnected', this.logContext);
      return;
    }
    if (mediaTrack.readyState === 'ended') {
      this.log.info('skipping incoming track as it already ended', this.logContext);
      return;
    }
    const parts = unpackStreamId(stream.id);
    const participantSid = parts[0];
    let streamId = parts[1];
    let trackId = mediaTrack.id;
    // firefox will get streamId (pID|trackId) instead of (pID|streamId) as it doesn't support sync tracks by stream
    // and generates its own track id instead of infer from sdp track id.
    if (streamId && streamId.startsWith('TR')) trackId = streamId;

    if (participantSid === this.localParticipant.sid) {
      this.log.warn('tried to create RemoteParticipant for local participant', this.logContext);
      return;
    }

    const participant = Array.from(this.remoteParticipants.values()).find(
      (p) => p.sid === participantSid,
    ) as RemoteParticipant | undefined;

    if (!participant) {
      this.log.error(
        `Tried to add a track for a participant, that's not present. Sid: ${participantSid}`,
        this.logContext,
      );
      return;
    }

    // in single peer connection case, the trackID is locally generated,
    // not the TR_ prefixed one generated by the server,
    // use `mid` to find the appropriate track.
    if (!trackId.startsWith('TR')) {
      const id = this.engine.getTrackIdForReceiver(receiver);
      if (!id) {
        this.log.error(
          `Tried to add a track whose 'sid' could not be found for a participant, that's not present. Sid: ${participantSid}`,
          this.logContext,
        );
        return;
      }

      trackId = id;
    }
    if (!trackId.startsWith('TR')) {
      this.log.warn(
        `Tried to add a track whose 'sid' could not be determined for a participant, that's not present. Sid: ${participantSid}, streamId: ${streamId}, trackId: ${trackId}`,
        { ...this.logContext, rpID: participantSid, streamId, trackId },
      );
    }

    let adaptiveStreamSettings: AdaptiveStreamSettings | undefined;
    if (this.options.adaptiveStream) {
      if (typeof this.options.adaptiveStream === 'object') {
        adaptiveStreamSettings = this.options.adaptiveStream;
      } else {
        adaptiveStreamSettings = {};
      }
    }

    const publication = participant.addSubscribedMediaTrack(
      mediaTrack,
      trackId,
      stream,
      receiver,
      adaptiveStreamSettings,
    );

    if (publication?.isEncrypted && !this.e2eeManager) {
      this.emit(
        RoomEvent.EncryptionError,
        new Error(
          `Encrypted ${publication.source} track received from participant ${participant.sid}, but room does not have encryption enabled!`,
        ),
      );
    }
  }

  private handleRestarting = () => {
    this.clearConnectionReconcile();
    // in case we went from resuming to full-reconnect, make sure to reflect it on the isResuming flag
    this.isResuming = false;

    // also unwind existing participants & existing subscriptions
    for (const p of this.remoteParticipants.values()) {
      this.handleParticipantDisconnected(p.identity, p);
    }

    if (this.setAndEmitConnectionState(ConnectionState.Reconnecting)) {
      this.emit(RoomEvent.Reconnecting);
    }
  };

  private handleSignalRestarted = async (joinResponse: JoinResponse) => {
    this.log.debug(`signal reconnected to server, region ${joinResponse.serverRegion}`, {
      ...this.logContext,
      region: joinResponse.serverRegion,
    });
    this.bufferedEvents = [];

    this.applyJoinResponse(joinResponse);

    try {
      // unpublish & republish tracks
      await this.localParticipant.republishAllTracks(undefined, true);
    } catch (error) {
      this.log.error('error trying to re-publish tracks after reconnection', {
        ...this.logContext,
        error,
      });
    }

    try {
      await this.engine.waitForRestarted();
      this.log.debug(`fully reconnected to server`, {
        ...this.logContext,
        region: joinResponse.serverRegion,
      });
    } catch {
      // reconnection failed, handleDisconnect is being invoked already, just return here
      return;
    }
    this.setAndEmitConnectionState(ConnectionState.Connected);
    this.emit(RoomEvent.Reconnected);
    this.registerConnectionReconcile();
    this.emitBufferedEvents();
  };

  private handleDisconnect(shouldStopTracks = true, reason?: DisconnectReason) {
    this.clearConnectionReconcile();
    this.isResuming = false;
    this.bufferedEvents = [];
    this.transcriptionReceivedTimes.clear();
    this.incomingDataStreamManager.clearHandlersAndControllers();
    if (this.state === ConnectionState.Disconnected) {
      return;
    }

    this.regionUrl = undefined;

    try {
      this.remoteParticipants.forEach((p) => {
        p.trackPublications.forEach((pub) => {
          p.unpublishTrack(pub.trackSid);
        });
      });

      this.localParticipant.trackPublications.forEach((pub) => {
        if (pub.track) {
          this.localParticipant.unpublishTrack(pub.track, shouldStopTracks);
        }
        if (shouldStopTracks) {
          pub.track?.detach();
          pub.track?.stop();
        } else {
          pub.track?.stopMonitor();
        }
      });

      this.localParticipant
        .off(ParticipantEvent.ParticipantMetadataChanged, this.onLocalParticipantMetadataChanged)
        .off(ParticipantEvent.ParticipantNameChanged, this.onLocalParticipantNameChanged)
        .off(ParticipantEvent.AttributesChanged, this.onLocalAttributesChanged)
        .off(ParticipantEvent.TrackMuted, this.onLocalTrackMuted)
        .off(ParticipantEvent.TrackUnmuted, this.onLocalTrackUnmuted)
        .off(ParticipantEvent.LocalTrackPublished, this.onLocalTrackPublished)
        .off(ParticipantEvent.LocalTrackUnpublished, this.onLocalTrackUnpublished)
        .off(ParticipantEvent.ConnectionQualityChanged, this.onLocalConnectionQualityChanged)
        .off(ParticipantEvent.MediaDevicesError, this.onMediaDevicesError)
        .off(ParticipantEvent.AudioStreamAcquired, this.startAudio)
        .off(ParticipantEvent.ChatMessage, this.onLocalChatMessageSent)
        .off(
          ParticipantEvent.ParticipantPermissionsChanged,
          this.onLocalParticipantPermissionsChanged,
        );

      this.localParticipant.trackPublications.clear();
      this.localParticipant.videoTrackPublications.clear();
      this.localParticipant.audioTrackPublications.clear();

      this.remoteParticipants.clear();
      this.sidToIdentity.clear();
      this.activeSpeakers = [];
      if (this.audioContext && typeof this.options.webAudioMix === 'boolean') {
        this.audioContext.close();
        this.audioContext = undefined;
      }
      if (isWeb()) {
        window.removeEventListener('beforeunload', this.onPageLeave);
        window.removeEventListener('pagehide', this.onPageLeave);
        window.removeEventListener('freeze', this.onPageLeave);
        navigator.mediaDevices?.removeEventListener('devicechange', this.handleDeviceChange);
      }
    } finally {
      this.setAndEmitConnectionState(ConnectionState.Disconnected);
      this.emit(RoomEvent.Disconnected, reason);
    }
  }

  private handleParticipantUpdates = (participantInfos: ParticipantInfo[]) => {
    // handle changes to participant state, and send events
    participantInfos.forEach((info) => {
      if (info.identity === this.localParticipant.identity) {
        this.localParticipant.updateInfo(info);
        return;
      }

      // LiveKit server doesn't send identity info prior to version 1.5.2 in disconnect updates
      // so we try to map an empty identity to an already known sID manually
      if (info.identity === '') {
        info.identity = this.sidToIdentity.get(info.sid) ?? '';
      }

      let remoteParticipant = this.remoteParticipants.get(info.identity);

      // when it's disconnected, send updates
      if (info.state === ParticipantInfo_State.DISCONNECTED) {
        this.handleParticipantDisconnected(info.identity, remoteParticipant);
      } else {
        // create participant if doesn't exist
        remoteParticipant = this.getOrCreateParticipant(info.identity, info);
      }
    });
  };

  private handleParticipantDisconnected(identity: string, participant?: RemoteParticipant) {
    // remove and send event
    this.remoteParticipants.delete(identity);
    if (!participant) {
      return;
    }

    this.incomingDataStreamManager.validateParticipantHasNoActiveDataStreams(identity);

    participant.trackPublications.forEach((publication) => {
      participant.unpublishTrack(publication.trackSid, true);
    });
    this.emit(RoomEvent.ParticipantDisconnected, participant);
    participant.setDisconnected();
    this.localParticipant?.handleParticipantDisconnected(participant.identity);
  }

  // updates are sent only when there's a change to speaker ordering
  private handleActiveSpeakersUpdate = (speakers: SpeakerInfo[]) => {
    const activeSpeakers: Participant[] = [];
    const seenSids: any = {};
    speakers.forEach((speaker) => {
      seenSids[speaker.sid] = true;
      if (speaker.sid === this.localParticipant.sid) {
        this.localParticipant.audioLevel = speaker.level;
        this.localParticipant.setIsSpeaking(true);
        activeSpeakers.push(this.localParticipant);
      } else {
        const p = this.getRemoteParticipantBySid(speaker.sid);
        if (p) {
          p.audioLevel = speaker.level;
          p.setIsSpeaking(true);
          activeSpeakers.push(p);
        }
      }
    });

    if (!seenSids[this.localParticipant.sid]) {
      this.localParticipant.audioLevel = 0;
      this.localParticipant.setIsSpeaking(false);
    }
    this.remoteParticipants.forEach((p) => {
      if (!seenSids[p.sid]) {
        p.audioLevel = 0;
        p.setIsSpeaking(false);
      }
    });

    this.activeSpeakers = activeSpeakers;
    this.emitWhenConnected(RoomEvent.ActiveSpeakersChanged, activeSpeakers);
  };

  // process list of changed speakers
  private handleSpeakersChanged = (speakerUpdates: SpeakerInfo[]) => {
    const lastSpeakers = new Map<string, Participant>();
    this.activeSpeakers.forEach((p) => {
      const remoteParticipant = this.remoteParticipants.get(p.identity);
      if (remoteParticipant && remoteParticipant.sid !== p.sid) {
        return;
      }
      lastSpeakers.set(p.sid, p);
    });
    speakerUpdates.forEach((speaker) => {
      let p: Participant | undefined = this.getRemoteParticipantBySid(speaker.sid);
      if (speaker.sid === this.localParticipant.sid) {
        p = this.localParticipant;
      }
      if (!p) {
        return;
      }
      p.audioLevel = speaker.level;
      p.setIsSpeaking(speaker.active);

      if (speaker.active) {
        lastSpeakers.set(speaker.sid, p);
      } else {
        lastSpeakers.delete(speaker.sid);
      }
    });
    const activeSpeakers = Array.from(lastSpeakers.values());
    activeSpeakers.sort((a, b) => b.audioLevel - a.audioLevel);
    this.activeSpeakers = activeSpeakers;
    this.emitWhenConnected(RoomEvent.ActiveSpeakersChanged, activeSpeakers);
  };

  private handleStreamStateUpdate = (streamStateUpdate: StreamStateUpdate) => {
    streamStateUpdate.streamStates.forEach((streamState) => {
      const participant = this.getRemoteParticipantBySid(streamState.participantSid);
      if (!participant) {
        return;
      }
      const pub = participant.getTrackPublicationBySid(streamState.trackSid);
      if (!pub || !pub.track) {
        return;
      }
      const newStreamState = Track.streamStateFromProto(streamState.state);
      pub.track.setStreamState(newStreamState);
      if (newStreamState !== pub.track.streamState) {
        participant.emit(ParticipantEvent.TrackStreamStateChanged, pub, pub.track.streamState);
        this.emitWhenConnected(
          RoomEvent.TrackStreamStateChanged,
          pub,
          pub.track.streamState,
          participant,
        );
      }
    });
  };

  private handleSubscriptionPermissionUpdate = (update: SubscriptionPermissionUpdate) => {
    const participant = this.getRemoteParticipantBySid(update.participantSid);
    if (!participant) {
      return;
    }
    const pub = participant.getTrackPublicationBySid(update.trackSid);
    if (!pub) {
      return;
    }

    pub.setAllowed(update.allowed);
  };

  private handleSubscriptionError = (update: SubscriptionResponse) => {
    const participant = Array.from(this.remoteParticipants.values()).find((p) =>
      p.trackPublications.has(update.trackSid),
    );
    if (!participant) {
      return;
    }
    const pub = participant.getTrackPublicationBySid(update.trackSid);
    if (!pub) {
      return;
    }

    pub.setSubscriptionError(update.err);
  };

  private handleDataPacket = (packet: DataPacket, encryptionType: Encryption_Type) => {
    // find the participant
    const participant = this.remoteParticipants.get(packet.participantIdentity);
    if (packet.value.case === 'user') {
      this.handleUserPacket(participant, packet.value.value, packet.kind, encryptionType);
    } else if (packet.value.case === 'transcription') {
      this.handleTranscription(participant, packet.value.value);
    } else if (packet.value.case === 'sipDtmf') {
      this.handleSipDtmf(participant, packet.value.value);
    } else if (packet.value.case === 'chatMessage') {
      this.handleChatMessage(participant, packet.value.value);
    } else if (packet.value.case === 'metrics') {
      this.handleMetrics(packet.value.value, participant);
    } else if (
      packet.value.case === 'streamHeader' ||
      packet.value.case === 'streamChunk' ||
      packet.value.case === 'streamTrailer'
    ) {
      this.handleDataStream(packet, encryptionType);
    } else if (packet.value.case === 'rpcRequest') {
      const rpc = packet.value.value;
      this.handleIncomingRpcRequest(
        packet.participantIdentity,
        rpc.id,
        rpc.method,
        rpc.payload,
        rpc.responseTimeoutMs,
        rpc.version,
      );
    }
  };

  private handleUserPacket = (
    participant: RemoteParticipant | undefined,
    userPacket: UserPacket,
    kind: DataPacket_Kind,
    encryptionType: Encryption_Type,
  ) => {
    this.emit(
      RoomEvent.DataReceived,
      userPacket.payload,
      participant,
      kind,
      userPacket.topic,
      encryptionType,
    );

    // also emit on the participant
    participant?.emit(ParticipantEvent.DataReceived, userPacket.payload, kind, encryptionType);
  };

  private handleSipDtmf = (participant: RemoteParticipant | undefined, dtmf: SipDTMF) => {
    this.emit(RoomEvent.SipDTMFReceived, dtmf, participant);

    // also emit on the participant
    participant?.emit(ParticipantEvent.SipDTMFReceived, dtmf);
  };

  private handleTranscription = (
    _remoteParticipant: RemoteParticipant | undefined,
    transcription: TranscriptionModel,
  ) => {
    // find the participant
    const participant =
      transcription.transcribedParticipantIdentity === this.localParticipant.identity
        ? this.localParticipant
        : this.getParticipantByIdentity(transcription.transcribedParticipantIdentity);
    const publication = participant?.trackPublications.get(transcription.trackId);

    const segments = extractTranscriptionSegments(transcription, this.transcriptionReceivedTimes);

    publication?.emit(TrackEvent.TranscriptionReceived, segments);
    participant?.emit(ParticipantEvent.TranscriptionReceived, segments, publication);
    this.emit(RoomEvent.TranscriptionReceived, segments, participant, publication);
  };

  private handleChatMessage = (
    participant: RemoteParticipant | undefined,
    chatMessage: ChatMessageModel,
  ) => {
    const msg = extractChatMessage(chatMessage);
    this.emit(RoomEvent.ChatMessage, msg, participant);
  };

  private handleMetrics = (metrics: MetricsBatch, participant?: Participant) => {
    this.emit(RoomEvent.MetricsReceived, metrics, participant);
  };

  private handleDataStream = (packet: DataPacket, encryptionType: Encryption_Type) => {
    this.incomingDataStreamManager.handleDataStreamPacket(packet, encryptionType);
  };

  private async handleIncomingRpcRequest(
    callerIdentity: string,
    requestId: string,
    method: string,
    payload: string,
    responseTimeout: number,
    version: number,
  ) {
    await this.engine.publishRpcAck(callerIdentity, requestId);

    if (version !== 1) {
      await this.engine.publishRpcResponse(
        callerIdentity,
        requestId,
        null,
        RpcError.builtIn('UNSUPPORTED_VERSION'),
      );
      return;
    }

    const handler = this.rpcHandlers.get(method);

    if (!handler) {
      await this.engine.publishRpcResponse(
        callerIdentity,
        requestId,
        null,
        RpcError.builtIn('UNSUPPORTED_METHOD'),
      );
      return;
    }

    let responseError: RpcError | null = null;
    let responsePayload: string | null = null;

    try {
      const response = await handler({
        requestId,
        callerIdentity,
        payload,
        responseTimeout,
      });
      if (byteLength(response) > MAX_PAYLOAD_BYTES) {
        responseError = RpcError.builtIn('RESPONSE_PAYLOAD_TOO_LARGE');
        console.warn(`RPC Response payload too large for ${method}`);
      } else {
        responsePayload = response;
      }
    } catch (error) {
      if (error instanceof RpcError) {
        responseError = error;
      } else {
        console.warn(
          `Uncaught error returned by RPC handler for ${method}. Returning APPLICATION_ERROR instead.`,
          error,
        );
        responseError = RpcError.builtIn('APPLICATION_ERROR');
      }
    }
    await this.engine.publishRpcResponse(callerIdentity, requestId, responsePayload, responseError);
  }

  bufferedSegments: Map<string, TranscriptionSegmentModel> = new Map();

  private handleAudioPlaybackStarted = () => {
    if (this.canPlaybackAudio) {
      return;
    }
    this.audioEnabled = true;
    this.emit(RoomEvent.AudioPlaybackStatusChanged, true);
  };

  private handleAudioPlaybackFailed = (e: any) => {
    this.log.warn('could not playback audio', { ...this.logContext, error: e });
    if (!this.canPlaybackAudio) {
      return;
    }
    this.audioEnabled = false;
    this.emit(RoomEvent.AudioPlaybackStatusChanged, false);
  };

  private handleVideoPlaybackStarted = () => {
    if (this.isVideoPlaybackBlocked) {
      this.isVideoPlaybackBlocked = false;
      this.emit(RoomEvent.VideoPlaybackStatusChanged, true);
    }
  };

  private handleVideoPlaybackFailed = () => {
    if (!this.isVideoPlaybackBlocked) {
      this.isVideoPlaybackBlocked = true;
      this.emit(RoomEvent.VideoPlaybackStatusChanged, false);
    }
  };

  /**
   * attempt to select the default devices if the previously selected devices are no longer available after a device change event
   */
  private async selectDefaultDevices() {
    const previousDevices = DeviceManager.getInstance().previousDevices;
    // check for available devices, but don't request permissions in order to avoid prompts for kinds that haven't been used before
    const availableDevices = await DeviceManager.getInstance().getDevices(undefined, false);
    const browser = getBrowser();
    if (browser?.name === 'Chrome' && browser.os !== 'iOS') {
      for (let availableDevice of availableDevices) {
        const previousDevice = previousDevices.find(
          (info) => info.deviceId === availableDevice.deviceId,
        );
        if (
          previousDevice &&
          previousDevice.label !== '' &&
          previousDevice.kind === availableDevice.kind &&
          previousDevice.label !== availableDevice.label
        ) {
          // label has changed on device the same deviceId, indicating that the default device has changed on the OS level
          if (this.getActiveDevice(availableDevice.kind) === 'default') {
            // emit an active device change event only if the selected output device is actually on `default`
            this.emit(
              RoomEvent.ActiveDeviceChanged,
              availableDevice.kind,
              availableDevice.deviceId,
            );
          }
        }
      }
    }

    const kinds: MediaDeviceKind[] = ['audiooutput', 'audioinput', 'videoinput'];
    for (let kind of kinds) {
      const targetSource = kindToSource(kind);
      const targetPublication = this.localParticipant.getTrackPublication(targetSource);
      if (targetPublication && targetPublication.track?.isUserProvided) {
        // if the track is user provided, we don't want to switch devices on behalf of the user
        continue;
      }
      const devicesOfKind = availableDevices.filter((d) => d.kind === kind);
      const activeDevice = this.getActiveDevice(kind);

      if (activeDevice === previousDevices.filter((info) => info.kind === kind)[0]?.deviceId) {
        // in  Safari the first device is always the default, so we assume a user on the default device would like to switch to the default once it changes
        // FF doesn't emit an event when the default device changes, so we perform the same best effort and switch to the new device once connected and if it's the first in the array
        if (devicesOfKind.length > 0 && devicesOfKind[0]?.deviceId !== activeDevice) {
          await this.switchActiveDevice(kind, devicesOfKind[0].deviceId);
          continue;
        }
      }

      if ((kind === 'audioinput' && !isSafariBased()) || kind === 'videoinput') {
        // airpods on Safari need special handling for audioinput as the track doesn't end as soon as you take them out
        continue;
      }
      // switch to first available device if previously active device is not available any more
      if (
        devicesOfKind.length > 0 &&
        !devicesOfKind.find((deviceInfo) => deviceInfo.deviceId === this.getActiveDevice(kind)) &&
        // avoid switching audio output on safari without explicit user action as it leads to slowed down audio playback
        (kind !== 'audiooutput' || !isSafariBased())
      ) {
        await this.switchActiveDevice(kind, devicesOfKind[0].deviceId);
      }
    }
  }

  private handleDeviceChange = async () => {
    if (getBrowser()?.os !== 'iOS') {
      // default devices are non deterministic on iOS, so we don't attempt to select them here
      await this.selectDefaultDevices();
    }
    this.emit(RoomEvent.MediaDevicesChanged);
  };

  private handleRoomUpdate = (room: RoomModel) => {
    const oldRoom = this.roomInfo;
    this.roomInfo = room;
    if (oldRoom && oldRoom.metadata !== room.metadata) {
      this.emitWhenConnected(RoomEvent.RoomMetadataChanged, room.metadata);
    }
    if (oldRoom?.activeRecording !== room.activeRecording) {
      this.emitWhenConnected(RoomEvent.RecordingStatusChanged, room.activeRecording);
    }
  };

  private handleConnectionQualityUpdate = (update: ConnectionQualityUpdate) => {
    update.updates.forEach((info) => {
      if (info.participantSid === this.localParticipant.sid) {
        this.localParticipant.setConnectionQuality(info.quality);
        return;
      }
      const participant = this.getRemoteParticipantBySid(info.participantSid);
      if (participant) {
        participant.setConnectionQuality(info.quality);
      }
    });
  };

  private async acquireAudioContext() {
    if (typeof this.options.webAudioMix !== 'boolean' && this.options.webAudioMix.audioContext) {
      // override audio context with custom audio context if supplied by user
      this.audioContext = this.options.webAudioMix.audioContext;
    } else if (!this.audioContext || this.audioContext.state === 'closed') {
      // by using an AudioContext, it reduces lag on audio elements
      // https://stackoverflow.com/questions/9811429/html5-audio-tag-on-safari-has-a-delay/54119854#54119854
      this.audioContext = getNewAudioContext() ?? undefined;
    }

    if (this.options.webAudioMix) {
      this.remoteParticipants.forEach((participant) =>
        participant.setAudioContext(this.audioContext),
      );
    }

    this.localParticipant.setAudioContext(this.audioContext);

    if (this.audioContext && this.audioContext.state === 'suspended') {
      // for iOS a newly created AudioContext is always in `suspended` state.
      // we try our best to resume the context here, if that doesn't work, we just continue with regular processing
      try {
        await Promise.race([this.audioContext.resume(), sleep(200)]);
      } catch (e: any) {
        this.log.warn('Could not resume audio context', { ...this.logContext, error: e });
      }
    }

    const newContextIsRunning = this.audioContext?.state === 'running';
    if (newContextIsRunning !== this.canPlaybackAudio) {
      this.audioEnabled = newContextIsRunning;
      this.emit(RoomEvent.AudioPlaybackStatusChanged, newContextIsRunning);
    }
  }

  private createParticipant(identity: string, info?: ParticipantInfo): RemoteParticipant {
    let participant: RemoteParticipant;
    if (info) {
      participant = RemoteParticipant.fromParticipantInfo(this.engine.client, info, {
        loggerContextCb: () => this.logContext,
        loggerName: this.options.loggerName,
      });
    } else {
      participant = new RemoteParticipant(
        this.engine.client,
        '',
        identity,
        undefined,
        undefined,
        undefined,
        {
          loggerContextCb: () => this.logContext,
          loggerName: this.options.loggerName,
        },
      );
    }
    if (this.options.webAudioMix) {
      participant.setAudioContext(this.audioContext);
    }
    if (this.options.audioOutput?.deviceId) {
      participant
        .setAudioOutput(this.options.audioOutput)
        .catch((e) => this.log.warn(`Could not set audio output: ${e.message}`, this.logContext));
    }
    return participant;
  }

  private getOrCreateParticipant(identity: string, info: ParticipantInfo): RemoteParticipant {
    if (this.remoteParticipants.has(identity)) {
      const existingParticipant = this.remoteParticipants.get(identity)!;
      if (info) {
        const wasUpdated = existingParticipant.updateInfo(info);
        if (wasUpdated) {
          this.sidToIdentity.set(info.sid, info.identity);
        }
      }
      return existingParticipant;
    }
    const participant = this.createParticipant(identity, info);
    this.remoteParticipants.set(identity, participant);

    this.sidToIdentity.set(info.sid, info.identity);
    // if we have valid info and the participant wasn't in the map before, we can assume the participant is new
    // firing here to make sure that `ParticipantConnected` fires before the initial track events
    this.emitWhenConnected(RoomEvent.ParticipantConnected, participant);

    // also forward events
    // trackPublished is only fired for tracks added after both local participant
    // and remote participant joined the room
    participant
      .on(ParticipantEvent.TrackPublished, (trackPublication: RemoteTrackPublication) => {
        this.emitWhenConnected(RoomEvent.TrackPublished, trackPublication, participant);
      })
      .on(
        ParticipantEvent.TrackSubscribed,
        (track: RemoteTrack, publication: RemoteTrackPublication) => {
          // monitor playback status
          if (track.kind === Track.Kind.Audio) {
            track.on(TrackEvent.AudioPlaybackStarted, this.handleAudioPlaybackStarted);
            track.on(TrackEvent.AudioPlaybackFailed, this.handleAudioPlaybackFailed);
          } else if (track.kind === Track.Kind.Video) {
            track.on(TrackEvent.VideoPlaybackFailed, this.handleVideoPlaybackFailed);
            track.on(TrackEvent.VideoPlaybackStarted, this.handleVideoPlaybackStarted);
          }
          this.emit(RoomEvent.TrackSubscribed, track, publication, participant);
        },
      )
      .on(ParticipantEvent.TrackUnpublished, (publication: RemoteTrackPublication) => {
        this.emit(RoomEvent.TrackUnpublished, publication, participant);
      })
      .on(
        ParticipantEvent.TrackUnsubscribed,
        (track: RemoteTrack, publication: RemoteTrackPublication) => {
          this.emit(RoomEvent.TrackUnsubscribed, track, publication, participant);
        },
      )
      .on(ParticipantEvent.TrackMuted, (pub: TrackPublication) => {
        this.emitWhenConnected(RoomEvent.TrackMuted, pub, participant);
      })
      .on(ParticipantEvent.TrackUnmuted, (pub: TrackPublication) => {
        this.emitWhenConnected(RoomEvent.TrackUnmuted, pub, participant);
      })
      .on(ParticipantEvent.ParticipantMetadataChanged, (metadata: string | undefined) => {
        this.emitWhenConnected(RoomEvent.ParticipantMetadataChanged, metadata, participant);
      })
      .on(ParticipantEvent.ParticipantNameChanged, (name) => {
        this.emitWhenConnected(RoomEvent.ParticipantNameChanged, name, participant);
      })
      .on(ParticipantEvent.AttributesChanged, (changedAttributes: Record<string, string>) => {
        this.emitWhenConnected(
          RoomEvent.ParticipantAttributesChanged,
          changedAttributes,
          participant,
        );
      })
      .on(ParticipantEvent.ConnectionQualityChanged, (quality: ConnectionQuality) => {
        this.emitWhenConnected(RoomEvent.ConnectionQualityChanged, quality, participant);
      })
      .on(
        ParticipantEvent.ParticipantPermissionsChanged,
        (prevPermissions?: ParticipantPermission) => {
          this.emitWhenConnected(
            RoomEvent.ParticipantPermissionsChanged,
            prevPermissions,
            participant,
          );
        },
      )
      .on(ParticipantEvent.TrackSubscriptionStatusChanged, (pub, status) => {
        this.emitWhenConnected(RoomEvent.TrackSubscriptionStatusChanged, pub, status, participant);
      })
      .on(ParticipantEvent.TrackSubscriptionFailed, (trackSid, error) => {
        this.emit(RoomEvent.TrackSubscriptionFailed, trackSid, participant, error);
      })
      .on(ParticipantEvent.TrackSubscriptionPermissionChanged, (pub, status) => {
        this.emitWhenConnected(
          RoomEvent.TrackSubscriptionPermissionChanged,
          pub,
          status,
          participant,
        );
      })
      .on(ParticipantEvent.Active, () => {
        this.emitWhenConnected(RoomEvent.ParticipantActive, participant);
        if (participant.kind === ParticipantKind.AGENT) {
          this.localParticipant.setActiveAgent(participant);
        }
      });

    // update info at the end after callbacks have been set up
    if (info) {
      participant.updateInfo(info);
    }
    return participant;
  }

  private sendSyncState() {
    const remoteTracks = Array.from(this.remoteParticipants.values()).reduce((acc, participant) => {
      acc.push(...(participant.getTrackPublications() as RemoteTrackPublication[])); // FIXME would be nice to have this return RemoteTrackPublications directly instead of the type cast
      return acc;
    }, [] as RemoteTrackPublication[]);
    const localTracks = this.localParticipant.getTrackPublications() as LocalTrackPublication[]; // FIXME would be nice to have this return LocalTrackPublications directly instead of the type cast
    this.engine.sendSyncState(remoteTracks, localTracks);
  }

  /**
   * After resuming, we'll need to notify the server of the current
   * subscription settings.
   */
  private updateSubscriptions() {
    for (const p of this.remoteParticipants.values()) {
      for (const pub of p.videoTrackPublications.values()) {
        if (pub.isSubscribed && isRemotePub(pub)) {
          pub.emitTrackUpdate();
        }
      }
    }
  }

  private getRemoteParticipantBySid(sid: string): RemoteParticipant | undefined {
    const identity = this.sidToIdentity.get(sid);
    if (identity) {
      return this.remoteParticipants.get(identity);
    }
  }

  private registerConnectionReconcile() {
    this.clearConnectionReconcile();
    let consecutiveFailures = 0;
    this.connectionReconcileInterval = CriticalTimers.setInterval(() => {
      if (
        // ensure we didn't tear it down
        !this.engine ||
        // engine detected close, but Room missed it
        this.engine.isClosed ||
        // transports failed without notifying engine
        !this.engine.verifyTransport()
      ) {
        consecutiveFailures++;
        this.log.warn('detected connection state mismatch', {
          ...this.logContext,
          numFailures: consecutiveFailures,
          engine: this.engine
            ? {
                closed: this.engine.isClosed,
                transportsConnected: this.engine.verifyTransport(),
              }
            : undefined,
        });
        if (consecutiveFailures >= 3) {
          this.recreateEngine();
          this.handleDisconnect(
            this.options.stopLocalTrackOnUnpublish,
            DisconnectReason.STATE_MISMATCH,
          );
        }
      } else {
        consecutiveFailures = 0;
      }
    }, connectionReconcileFrequency);
  }

  private clearConnectionReconcile() {
    if (this.connectionReconcileInterval) {
      CriticalTimers.clearInterval(this.connectionReconcileInterval);
    }
  }

  private setAndEmitConnectionState(state: ConnectionState): boolean {
    if (state === this.state) {
      // unchanged
      return false;
    }
    this.state = state;
    this.emit(RoomEvent.ConnectionStateChanged, this.state);
    return true;
  }

  private emitBufferedEvents() {
    this.bufferedEvents.forEach(([ev, args]) => {
      this.emit(ev, ...args);
    });
    this.bufferedEvents = [];
  }

  private emitWhenConnected<E extends keyof RoomEventCallbacks>(
    event: E,
    ...args: Parameters<RoomEventCallbacks[E]>
  ): boolean {
    if (
      this.state === ConnectionState.Reconnecting ||
      this.isResuming ||
      !this.engine ||
      this.engine.pendingReconnect
    ) {
      // in case the room is reconnecting, buffer the events by firing them later after emitting RoomEvent.Reconnected
      this.bufferedEvents.push([event, args]);
    } else if (this.state === ConnectionState.Connected) {
      return this.emit(event, ...args);
    }
    return false;
  }

  private onLocalParticipantMetadataChanged = (metadata: string | undefined) => {
    this.emit(RoomEvent.ParticipantMetadataChanged, metadata, this.localParticipant);
  };

  private onLocalParticipantNameChanged = (name: string) => {
    this.emit(RoomEvent.ParticipantNameChanged, name, this.localParticipant);
  };

  private onLocalAttributesChanged = (changedAttributes: Record<string, string>) => {
    this.emit(RoomEvent.ParticipantAttributesChanged, changedAttributes, this.localParticipant);
  };

  private onLocalTrackMuted = (pub: TrackPublication) => {
    this.emit(RoomEvent.TrackMuted, pub, this.localParticipant);
  };

  private onLocalTrackUnmuted = (pub: TrackPublication) => {
    this.emit(RoomEvent.TrackUnmuted, pub, this.localParticipant);
  };

  private onTrackProcessorUpdate = (processor?: TrackProcessor<Track.Kind, any>) => {
    processor?.onPublish?.(this);
  };

  private onLocalTrackPublished = async (pub: LocalTrackPublication) => {
    pub.track?.on(TrackEvent.TrackProcessorUpdate, this.onTrackProcessorUpdate);
    pub.track?.on(TrackEvent.Restarted, this.onLocalTrackRestarted);
    pub.track?.getProcessor()?.onPublish?.(this);

    this.emit(RoomEvent.LocalTrackPublished, pub, this.localParticipant);

    if (isLocalAudioTrack(pub.track)) {
      const trackIsSilent = await pub.track.checkForSilence();
      if (trackIsSilent) {
        this.emit(RoomEvent.LocalAudioSilenceDetected, pub);
      }
    }
    const deviceId = await pub.track?.getDeviceId(false);
    const deviceKind = sourceToKind(pub.source);
    if (
      deviceKind &&
      deviceId &&
      deviceId !== this.localParticipant.activeDeviceMap.get(deviceKind)
    ) {
      this.localParticipant.activeDeviceMap.set(deviceKind, deviceId);
      this.emit(RoomEvent.ActiveDeviceChanged, deviceKind, deviceId);
    }
  };

  private onLocalTrackUnpublished = (pub: LocalTrackPublication) => {
    pub.track?.off(TrackEvent.TrackProcessorUpdate, this.onTrackProcessorUpdate);
    pub.track?.off(TrackEvent.Restarted, this.onLocalTrackRestarted);
    this.emit(RoomEvent.LocalTrackUnpublished, pub, this.localParticipant);
  };

  private onLocalTrackRestarted = async (track: LocalTrack) => {
    const deviceId = await track.getDeviceId(false);
    const deviceKind = sourceToKind(track.source);
    if (
      deviceKind &&
      deviceId &&
      deviceId !== this.localParticipant.activeDeviceMap.get(deviceKind)
    ) {
      this.log.debug(
        `local track restarted, setting ${deviceKind} ${deviceId} active`,
        this.logContext,
      );
      this.localParticipant.activeDeviceMap.set(deviceKind, deviceId);
      this.emit(RoomEvent.ActiveDeviceChanged, deviceKind, deviceId);
    }
  };

  private onLocalConnectionQualityChanged = (quality: ConnectionQuality) => {
    this.emit(RoomEvent.ConnectionQualityChanged, quality, this.localParticipant);
  };

  private onMediaDevicesError = (e: Error, kind?: MediaDeviceKind) => {
    this.emit(RoomEvent.MediaDevicesError, e, kind);
  };

  private onLocalParticipantPermissionsChanged = (prevPermissions?: ParticipantPermission) => {
    this.emit(RoomEvent.ParticipantPermissionsChanged, prevPermissions, this.localParticipant);
  };

  private onLocalChatMessageSent = (msg: ChatMessage) => {
    this.emit(RoomEvent.ChatMessage, msg, this.localParticipant);
  };

  /**
   * Allows to populate a room with simulated participants.
   * No actual connection to a server will be established, all state is
   * @experimental
   */
  async simulateParticipants(options: SimulationOptions) {
    const publishOptions = {
      audio: true,
      video: true,
      useRealTracks: false,
      ...options.publish,
    };
    const participantOptions = {
      count: 9,
      audio: false,
      video: true,
      aspectRatios: [1.66, 1.7, 1.3],
      ...options.participants,
    };
    this.handleDisconnect();
    this.roomInfo = new RoomModel({
      sid: 'RM_SIMULATED',
      name: 'simulated-room',
      emptyTimeout: 0,
      maxParticipants: 0,
      creationTime: protoInt64.parse(new Date().getTime()),
      metadata: '',
      numParticipants: 1,
      numPublishers: 1,
      turnPassword: '',
      enabledCodecs: [],
      activeRecording: false,
    });

    this.localParticipant.updateInfo(
      new ParticipantInfo({
        identity: 'simulated-local',
        name: 'local-name',
      }),
    );
    this.setupLocalParticipantEvents();
    this.emit(RoomEvent.SignalConnected);
    this.emit(RoomEvent.Connected);
    this.setAndEmitConnectionState(ConnectionState.Connected);
    if (publishOptions.video) {
      const camPub = new LocalTrackPublication(
        Track.Kind.Video,
        new TrackInfo({
          source: TrackSource.CAMERA,
          sid: Math.floor(Math.random() * 10_000).toString(),
          type: TrackType.AUDIO,
          name: 'video-dummy',
        }),
        new LocalVideoTrack(
          publishOptions.useRealTracks
            ? (
                await window.navigator.mediaDevices.getUserMedia({ video: true })
              ).getVideoTracks()[0]
            : createDummyVideoStreamTrack(
                160 * (participantOptions.aspectRatios[0] ?? 1),
                160,
                true,
                true,
              ),
          undefined,
          false,
          { loggerName: this.options.loggerName, loggerContextCb: () => this.logContext },
        ),
        { loggerName: this.options.loggerName, loggerContextCb: () => this.logContext },
      );
      // @ts-ignore
      this.localParticipant.addTrackPublication(camPub);
      this.localParticipant.emit(ParticipantEvent.LocalTrackPublished, camPub);
    }
    if (publishOptions.audio) {
      const audioPub = new LocalTrackPublication(
        Track.Kind.Audio,
        new TrackInfo({
          source: TrackSource.MICROPHONE,
          sid: Math.floor(Math.random() * 10_000).toString(),
          type: TrackType.AUDIO,
        }),
        new LocalAudioTrack(
          publishOptions.useRealTracks
            ? (await navigator.mediaDevices.getUserMedia({ audio: true })).getAudioTracks()[0]
            : getEmptyAudioStreamTrack(),
          undefined,
          false,
          this.audioContext,
          { loggerName: this.options.loggerName, loggerContextCb: () => this.logContext },
        ),
        { loggerName: this.options.loggerName, loggerContextCb: () => this.logContext },
      );
      // @ts-ignore
      this.localParticipant.addTrackPublication(audioPub);
      this.localParticipant.emit(ParticipantEvent.LocalTrackPublished, audioPub);
    }

    for (let i = 0; i < participantOptions.count - 1; i += 1) {
      let info: ParticipantInfo = new ParticipantInfo({
        sid: Math.floor(Math.random() * 10_000).toString(),
        identity: `simulated-${i}`,
        state: ParticipantInfo_State.ACTIVE,
        tracks: [],
        joinedAt: protoInt64.parse(Date.now()),
      });
      const p = this.getOrCreateParticipant(info.identity, info);
      if (participantOptions.video) {
        const dummyVideo = createDummyVideoStreamTrack(
          160 * (participantOptions.aspectRatios[i % participantOptions.aspectRatios.length] ?? 1),
          160,
          false,
          true,
        );
        const videoTrack = new TrackInfo({
          source: TrackSource.CAMERA,
          sid: Math.floor(Math.random() * 10_000).toString(),
          type: TrackType.AUDIO,
        });
        p.addSubscribedMediaTrack(
          dummyVideo,
          videoTrack.sid,
          new MediaStream([dummyVideo]),
          new RTCRtpReceiver(),
        );
        info.tracks = [...info.tracks, videoTrack];
      }
      if (participantOptions.audio) {
        const dummyTrack = getEmptyAudioStreamTrack();
        const audioTrack = new TrackInfo({
          source: TrackSource.MICROPHONE,
          sid: Math.floor(Math.random() * 10_000).toString(),
          type: TrackType.AUDIO,
        });
        p.addSubscribedMediaTrack(
          dummyTrack,
          audioTrack.sid,
          new MediaStream([dummyTrack]),
          new RTCRtpReceiver(),
        );
        info.tracks = [...info.tracks, audioTrack];
      }

      p.updateInfo(info);
    }
  }

  // /** @internal */
  emit<E extends keyof RoomEventCallbacks>(
    event: E,
    ...args: Parameters<RoomEventCallbacks[E]>
  ): boolean {
    // active speaker updates are too spammy
    if (event !== RoomEvent.ActiveSpeakersChanged && event !== RoomEvent.TranscriptionReceived) {
      // only extract logContext from arguments in order to avoid logging the whole object tree
      const minimizedArgs = mapArgs(args).filter((arg: unknown) => arg !== undefined);
      if (event === RoomEvent.TrackSubscribed || event === RoomEvent.TrackUnsubscribed) {
        this.log.trace(`subscribe trace: ${event}`, {
          ...this.logContext,
          event,
          args: minimizedArgs,
        });
      }
      this.log.debug(`room event ${event}`, { ...this.logContext, event, args: minimizedArgs });
    }
    return super.emit(event, ...args);
  }
}

function mapArgs(args: unknown[]): any {
  return args.map((arg: unknown) => {
    if (!arg) {
      return;
    }
    if (Array.isArray(arg)) {
      return mapArgs(arg);
    }
    if (typeof arg === 'object') {
      return 'logContext' in arg ? arg.logContext : undefined;
    }
    return arg;
  });
}

export default Room;

export type RoomEventCallbacks = {
  connected: () => void;
  reconnecting: () => void;
  signalReconnecting: () => void;
  reconnected: () => void;
  disconnected: (reason?: DisconnectReason) => void;
  connectionStateChanged: (state: ConnectionState) => void;
  moved: (name: string) => void;
  mediaDevicesChanged: () => void;
  participantConnected: (participant: RemoteParticipant) => void;
  participantDisconnected: (participant: RemoteParticipant) => void;
  trackPublished: (publication: RemoteTrackPublication, participant: RemoteParticipant) => void;
  trackSubscribed: (
    track: RemoteTrack,
    publication: RemoteTrackPublication,
    participant: RemoteParticipant,
  ) => void;
  trackSubscriptionFailed: (
    trackSid: string,
    participant: RemoteParticipant,
    reason?: SubscriptionError,
  ) => void;
  trackUnpublished: (publication: RemoteTrackPublication, participant: RemoteParticipant) => void;
  trackUnsubscribed: (
    track: RemoteTrack,
    publication: RemoteTrackPublication,
    participant: RemoteParticipant,
  ) => void;
  trackMuted: (publication: TrackPublication, participant: Participant) => void;
  trackUnmuted: (publication: TrackPublication, participant: Participant) => void;
  localTrackPublished: (publication: LocalTrackPublication, participant: LocalParticipant) => void;
  localTrackUnpublished: (
    publication: LocalTrackPublication,
    participant: LocalParticipant,
  ) => void;
  localAudioSilenceDetected: (publication: LocalTrackPublication) => void;
  participantMetadataChanged: (
    metadata: string | undefined,
    participant: RemoteParticipant | LocalParticipant,
  ) => void;
  participantNameChanged: (name: string, participant: RemoteParticipant | LocalParticipant) => void;
  participantPermissionsChanged: (
    prevPermissions: ParticipantPermission | undefined,
    participant: RemoteParticipant | LocalParticipant,
  ) => void;
  participantAttributesChanged: (
    changedAttributes: Record<string, string>,
    participant: RemoteParticipant | LocalParticipant,
  ) => void;
  activeSpeakersChanged: (speakers: Array<Participant>) => void;
  roomMetadataChanged: (metadata: string) => void;
  dataReceived: (
    payload: Uint8Array,
    participant?: RemoteParticipant,
    kind?: DataPacket_Kind,
    topic?: string,
    encryptionType?: Encryption_Type,
  ) => void;
  sipDTMFReceived: (dtmf: SipDTMF, participant?: RemoteParticipant) => void;
  transcriptionReceived: (
    transcription: TranscriptionSegment[],
    participant?: Participant,
    publication?: TrackPublication,
  ) => void;
  connectionQualityChanged: (quality: ConnectionQuality, participant: Participant) => void;
  mediaDevicesError: (error: Error, kind?: MediaDeviceKind) => void;
  trackStreamStateChanged: (
    publication: RemoteTrackPublication,
    streamState: Track.StreamState,
    participant: RemoteParticipant,
  ) => void;
  trackSubscriptionPermissionChanged: (
    publication: RemoteTrackPublication,
    status: TrackPublication.PermissionStatus,
    participant: RemoteParticipant,
  ) => void;
  trackSubscriptionStatusChanged: (
    publication: RemoteTrackPublication,
    status: TrackPublication.SubscriptionStatus,
    participant: RemoteParticipant,
  ) => void;
  audioPlaybackChanged: (playing: boolean) => void;
  videoPlaybackChanged: (playing: boolean) => void;
  signalConnected: () => void;
  recordingStatusChanged: (recording: boolean) => void;
  participantEncryptionStatusChanged: (encrypted: boolean, participant?: Participant) => void;
  encryptionError: (error: Error) => void;
  dcBufferStatusChanged: (isLow: boolean, kind: DataPacket_Kind) => void;
  activeDeviceChanged: (kind: MediaDeviceKind, deviceId: string) => void;
  chatMessage: (message: ChatMessage, participant?: RemoteParticipant | LocalParticipant) => void;
  localTrackSubscribed: (publication: LocalTrackPublication, participant: LocalParticipant) => void;
  metricsReceived: (metrics: MetricsBatch, participant?: Participant) => void;
  participantActive: (participant: Participant) => void;
};
