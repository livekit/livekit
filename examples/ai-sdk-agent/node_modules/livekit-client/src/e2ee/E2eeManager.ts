import { Encryption_Type, TrackInfo } from '@livekit/protocol';
import { EventEmitter } from 'events';
import type TypedEventEmitter from 'typed-emitter';
import log, { LogLevel, workerLogger } from '../logger';
import type RTCEngine from '../room/RTCEngine';
import type Room from '../room/Room';
import { ConnectionState } from '../room/Room';
import { DeviceUnsupportedError } from '../room/errors';
import { EngineEvent, ParticipantEvent, RoomEvent } from '../room/events';
import type RemoteTrack from '../room/track/RemoteTrack';
import type { Track } from '../room/track/Track';
import type { VideoCodec } from '../room/track/options';
import { mimeTypeToVideoCodecString } from '../room/track/utils';
import { Future, isChromiumBased, isLocalTrack, isSafariBased, isVideoTrack } from '../room/utils';
import type { BaseKeyProvider } from './KeyProvider';
import { E2EE_FLAG } from './constants';
import { type E2EEManagerCallbacks, EncryptionEvent, KeyProviderEvent } from './events';
import type {
  DecryptDataRequestMessage,
  DecryptDataResponseMessage,
  E2EEManagerOptions,
  E2EEWorkerMessage,
  EnableMessage,
  EncodeMessage,
  EncryptDataRequestMessage,
  EncryptDataResponseMessage,
  InitMessage,
  KeyInfo,
  RTPVideoMapMessage,
  RatchetRequestMessage,
  RemoveTransformMessage,
  ScriptTransformOptions,
  SetKeyMessage,
  SifTrailerMessage,
  UpdateCodecMessage,
} from './types';
import { isE2EESupported, isScriptTransformSupported } from './utils';

export interface BaseE2EEManager {
  setup(room: Room): void;
  setupEngine(engine: RTCEngine): void;
  isEnabled: boolean;
  isDataChannelEncryptionEnabled: boolean;
  setParticipantCryptorEnabled(enabled: boolean, participantIdentity: string): void;
  setSifTrailer(trailer: Uint8Array): void;
  encryptData(data: Uint8Array): Promise<EncryptDataResponseMessage['data']>;
  handleEncryptedData(
    payload: Uint8Array,
    iv: Uint8Array,
    participantIdentity: string,
    keyIndex: number,
  ): Promise<DecryptDataResponseMessage['data']>;
  on<E extends keyof E2EEManagerCallbacks>(event: E, listener: E2EEManagerCallbacks[E]): this;
}

/**
 * @experimental
 */
export class E2EEManager
  extends (EventEmitter as new () => TypedEventEmitter<E2EEManagerCallbacks>)
  implements BaseE2EEManager
{
  protected worker: Worker;

  protected room?: Room;

  private encryptionEnabled: boolean;

  private keyProvider: BaseKeyProvider;

  private decryptDataRequests: Map<string, Future<DecryptDataResponseMessage['data']>> = new Map();

  private encryptDataRequests: Map<string, Future<EncryptDataResponseMessage['data']>> = new Map();

  private dataChannelEncryptionEnabled: boolean;

  constructor(options: E2EEManagerOptions, dcEncryptionEnabled: boolean) {
    super();
    this.keyProvider = options.keyProvider;
    this.worker = options.worker;
    this.encryptionEnabled = false;
    this.dataChannelEncryptionEnabled = dcEncryptionEnabled;
  }

  get isEnabled(): boolean {
    return this.encryptionEnabled;
  }

  get isDataChannelEncryptionEnabled(): boolean {
    return this.isEnabled && this.dataChannelEncryptionEnabled;
  }

  /**
   * @internal
   */
  setup(room: Room) {
    if (!isE2EESupported()) {
      throw new DeviceUnsupportedError(
        'tried to setup end-to-end encryption on an unsupported browser',
      );
    }
    log.info('setting up e2ee');
    if (room !== this.room) {
      this.room = room;
      this.setupEventListeners(room, this.keyProvider);
      // this.worker = new Worker('');
      const msg: InitMessage = {
        kind: 'init',
        data: {
          keyProviderOptions: this.keyProvider.getOptions(),
          loglevel: workerLogger.getLevel() as LogLevel,
        },
      };
      if (this.worker) {
        log.info(`initializing worker`, { worker: this.worker });
        this.worker.onmessage = this.onWorkerMessage;
        this.worker.onerror = this.onWorkerError;
        this.worker.postMessage(msg);
      }
    }
  }

  /**
   * @internal
   */
  setParticipantCryptorEnabled(enabled: boolean, participantIdentity: string) {
    log.debug(`set e2ee to ${enabled} for participant ${participantIdentity}`);
    this.postEnable(enabled, participantIdentity);
  }

  /**
   * @internal
   */
  setSifTrailer(trailer: Uint8Array) {
    if (!trailer || trailer.length === 0) {
      log.warn("ignoring server sent trailer as it's empty");
    } else {
      this.postSifTrailer(trailer);
    }
  }

  private onWorkerMessage = (ev: MessageEvent<E2EEWorkerMessage>) => {
    const { kind, data } = ev.data;
    switch (kind) {
      case 'error':
        log.error(data.error.message);
        this.emit(EncryptionEvent.EncryptionError, data.error);
        break;
      case 'initAck':
        if (data.enabled) {
          this.keyProvider.getKeys().forEach((keyInfo) => {
            this.postKey(keyInfo);
          });
        }
        break;

      case 'enable':
        if (data.enabled) {
          this.keyProvider.getKeys().forEach((keyInfo) => {
            this.postKey(keyInfo);
          });
        }
        if (
          this.encryptionEnabled !== data.enabled &&
          data.participantIdentity === this.room?.localParticipant.identity
        ) {
          this.emit(
            EncryptionEvent.ParticipantEncryptionStatusChanged,
            data.enabled,
            this.room!.localParticipant,
          );
          this.encryptionEnabled = data.enabled;
        } else if (data.participantIdentity) {
          const participant = this.room?.getParticipantByIdentity(data.participantIdentity);
          if (!participant) {
            throw TypeError(
              `couldn't set encryption status, participant not found${data.participantIdentity}`,
            );
          }
          this.emit(EncryptionEvent.ParticipantEncryptionStatusChanged, data.enabled, participant);
        }
        break;
      case 'ratchetKey':
        this.keyProvider.emit(
          KeyProviderEvent.KeyRatcheted,
          data.ratchetResult,
          data.participantIdentity,
          data.keyIndex,
        );
        break;

      case 'decryptDataResponse':
        const decryptFuture = this.decryptDataRequests.get(data.uuid);
        if (decryptFuture?.resolve) {
          decryptFuture.resolve(data);
        }
        break;
      case 'encryptDataResponse':
        const encryptFuture = this.encryptDataRequests.get(data.uuid);
        if (encryptFuture?.resolve) {
          encryptFuture.resolve(data as EncryptDataResponseMessage['data']);
        }
        break;
      default:
        break;
    }
  };

  private onWorkerError = (ev: ErrorEvent) => {
    log.error('e2ee worker encountered an error:', { error: ev.error });
    this.emit(EncryptionEvent.EncryptionError, ev.error);
  };

  public setupEngine(engine: RTCEngine) {
    engine.on(EngineEvent.RTPVideoMapUpdate, (rtpMap) => {
      this.postRTPMap(rtpMap);
    });
  }

  private setupEventListeners(room: Room, keyProvider: BaseKeyProvider) {
    room.on(RoomEvent.TrackPublished, (pub, participant) =>
      this.setParticipantCryptorEnabled(
        pub.trackInfo!.encryption !== Encryption_Type.NONE,
        participant.identity,
      ),
    );
    room
      .on(RoomEvent.ConnectionStateChanged, (state) => {
        if (state === ConnectionState.Connected) {
          room.remoteParticipants.forEach((participant) => {
            participant.trackPublications.forEach((pub) => {
              this.setParticipantCryptorEnabled(
                pub.trackInfo!.encryption !== Encryption_Type.NONE,
                participant.identity,
              );
            });
          });
        }
      })
      .on(RoomEvent.TrackUnsubscribed, (track, _, participant) => {
        const msg: RemoveTransformMessage = {
          kind: 'removeTransform',
          data: {
            participantIdentity: participant.identity,
            trackId: track.mediaStreamID,
          },
        };
        this.worker?.postMessage(msg);
      })
      .on(RoomEvent.TrackSubscribed, (track, pub, participant) => {
        this.setupE2EEReceiver(track, participant.identity, pub.trackInfo);
      })
      .on(RoomEvent.SignalConnected, () => {
        if (!this.room) {
          throw new TypeError(`expected room to be present on signal connect`);
        }
        keyProvider.getKeys().forEach((keyInfo) => {
          this.postKey(keyInfo);
        });
        this.setParticipantCryptorEnabled(
          this.room.localParticipant.isE2EEEnabled,
          this.room.localParticipant.identity,
        );
      });

    room.localParticipant.on(ParticipantEvent.LocalSenderCreated, async (sender, track) => {
      this.setupE2EESender(track, sender);
    });

    room.localParticipant.on(ParticipantEvent.LocalTrackPublished, (publication) => {
      // Safari doesn't support retrieving payload information on RTCEncodedVideoFrame, so we need to update the codec manually once we have the trackInfo from the server
      if (!isVideoTrack(publication.track) || !isSafariBased()) {
        return;
      }
      const msg: UpdateCodecMessage = {
        kind: 'updateCodec',
        data: {
          trackId: publication.track!.mediaStreamID,
          codec: mimeTypeToVideoCodecString(publication.trackInfo!.codecs[0].mimeType),
          participantIdentity: this.room!.localParticipant.identity,
        },
      };

      this.worker.postMessage(msg);
    });

    keyProvider
      .on(KeyProviderEvent.SetKey, (keyInfo) => this.postKey(keyInfo))
      .on(KeyProviderEvent.RatchetRequest, (participantId, keyIndex) =>
        this.postRatchetRequest(participantId, keyIndex),
      );
  }

  async encryptData(data: Uint8Array): Promise<EncryptDataResponseMessage['data']> {
    if (!this.worker) {
      throw Error('could not encrypt data, worker is missing');
    }
    const uuid = crypto.randomUUID();
    const msg: EncryptDataRequestMessage = {
      kind: 'encryptDataRequest',
      data: {
        uuid,
        payload: data,
        participantIdentity: this.room!.localParticipant.identity,
      },
    };
    const future = new Future<EncryptDataResponseMessage['data']>();
    future.onFinally = () => {
      this.encryptDataRequests.delete(uuid);
    };
    this.encryptDataRequests.set(uuid, future);
    this.worker.postMessage(msg);
    return future!.promise!;
  }

  handleEncryptedData(
    payload: Uint8Array,
    iv: Uint8Array,
    participantIdentity: string,
    keyIndex: number,
  ) {
    if (!this.worker) {
      throw Error('could not handle encrypted data, worker is missing');
    }
    const uuid = crypto.randomUUID();
    const msg: DecryptDataRequestMessage = {
      kind: 'decryptDataRequest',
      data: {
        uuid,
        payload,
        iv,
        participantIdentity,
        keyIndex,
      },
    };
    const future = new Future<DecryptDataResponseMessage['data']>();
    future.onFinally = () => {
      this.decryptDataRequests.delete(uuid);
    };
    this.decryptDataRequests.set(uuid, future);
    this.worker.postMessage(msg);
    return future.promise;
  }

  private postRatchetRequest(participantIdentity?: string, keyIndex?: number) {
    if (!this.worker) {
      throw Error('could not ratchet key, worker is missing');
    }
    const msg: RatchetRequestMessage = {
      kind: 'ratchetRequest',
      data: {
        participantIdentity: participantIdentity,
        keyIndex,
      },
    };
    this.worker.postMessage(msg);
  }

  private postKey({ key, participantIdentity, keyIndex }: KeyInfo) {
    if (!this.worker) {
      throw Error('could not set key, worker is missing');
    }
    const msg: SetKeyMessage = {
      kind: 'setKey',
      data: {
        participantIdentity: participantIdentity,
        isPublisher: participantIdentity === this.room?.localParticipant.identity,
        key,
        keyIndex,
      },
    };
    this.worker.postMessage(msg);
  }

  private postEnable(enabled: boolean, participantIdentity: string) {
    if (this.worker) {
      const enableMsg: EnableMessage = {
        kind: 'enable',
        data: {
          enabled,
          participantIdentity,
        },
      };
      this.worker.postMessage(enableMsg);
    } else {
      throw new ReferenceError('failed to enable e2ee, worker is not ready');
    }
  }

  private postRTPMap(map: Map<number, VideoCodec>) {
    if (!this.worker) {
      throw TypeError('could not post rtp map, worker is missing');
    }
    if (!this.room?.localParticipant.identity) {
      throw TypeError('could not post rtp map, local participant identity is missing');
    }
    const msg: RTPVideoMapMessage = {
      kind: 'setRTPMap',
      data: {
        map,
        participantIdentity: this.room.localParticipant.identity,
      },
    };
    this.worker.postMessage(msg);
  }

  private postSifTrailer(trailer: Uint8Array) {
    if (!this.worker) {
      throw Error('could not post SIF trailer, worker is missing');
    }
    const msg: SifTrailerMessage = {
      kind: 'setSifTrailer',
      data: {
        trailer,
      },
    };
    this.worker.postMessage(msg);
  }

  private setupE2EEReceiver(track: RemoteTrack, remoteId: string, trackInfo?: TrackInfo) {
    if (!track.receiver) {
      return;
    }
    if (!trackInfo?.mimeType || trackInfo.mimeType === '') {
      throw new TypeError('MimeType missing from trackInfo, cannot set up E2EE cryptor');
    }
    this.handleReceiver(
      track.receiver,
      track.mediaStreamID,
      remoteId,
      track.kind === 'video' ? mimeTypeToVideoCodecString(trackInfo.mimeType) : undefined,
    );
  }

  private setupE2EESender(track: Track, sender: RTCRtpSender) {
    if (!isLocalTrack(track) || !sender) {
      if (!sender) log.warn('early return because sender is not ready');
      return;
    }
    this.handleSender(sender, track.mediaStreamID, undefined);
  }

  /**
   * Handles the given {@code RTCRtpReceiver} by creating a {@code TransformStream} which will inject
   * a frame decoder.
   *
   */
  private async handleReceiver(
    receiver: RTCRtpReceiver,
    trackId: string,
    participantIdentity: string,
    codec?: VideoCodec,
  ) {
    if (!this.worker) {
      return;
    }

    if (
      isScriptTransformSupported() &&
      // Chrome occasionally throws an `InvalidState` error when using script transforms directly after introducing this API in 141.
      // Disabling it for Chrome based browsers until the API has stabilized
      !isChromiumBased()
    ) {
      const options: ScriptTransformOptions = {
        kind: 'decode',
        participantIdentity,
        trackId,
        codec,
      };
      // @ts-ignore
      receiver.transform = new RTCRtpScriptTransform(this.worker, options);
    } else {
      if (E2EE_FLAG in receiver && codec) {
        // only update codec
        const msg: UpdateCodecMessage = {
          kind: 'updateCodec',
          data: {
            trackId,
            codec,
            participantIdentity: participantIdentity,
          },
        };
        this.worker.postMessage(msg);
        return;
      }
      // @ts-ignore
      let writable: WritableStream = receiver.writableStream;
      // @ts-ignore
      let readable: ReadableStream = receiver.readableStream;

      if (!writable || !readable) {
        // @ts-ignore
        const receiverStreams = receiver.createEncodedStreams();
        // @ts-ignore
        receiver.writableStream = receiverStreams.writable;
        writable = receiverStreams.writable;
        // @ts-ignore
        receiver.readableStream = receiverStreams.readable;
        readable = receiverStreams.readable;
      }

      const msg: EncodeMessage = {
        kind: 'decode',
        data: {
          readableStream: readable,
          writableStream: writable,
          trackId: trackId,
          codec,
          participantIdentity: participantIdentity,
          isReuse: E2EE_FLAG in receiver,
        },
      };
      this.worker.postMessage(msg, [readable, writable]);
    }

    // @ts-ignore
    receiver[E2EE_FLAG] = true;
  }

  /**
   * Handles the given {@code RTCRtpSender} by creating a {@code TransformStream} which will inject
   * a frame encoder.
   *
   */
  private handleSender(sender: RTCRtpSender, trackId: string, codec?: VideoCodec) {
    if (E2EE_FLAG in sender || !this.worker) {
      return;
    }

    if (!this.room?.localParticipant.identity || this.room.localParticipant.identity === '') {
      throw TypeError('local identity needs to be known in order to set up encrypted sender');
    }

    if (
      isScriptTransformSupported() &&
      // Chrome occasionally throws an `InvalidState` error when using script transforms directly after introducing this API in 141.
      // Disabling it for Chrome based browsers until the API has stabilized
      !isChromiumBased()
    ) {
      log.info('initialize script transform');
      const options = {
        kind: 'encode',
        participantIdentity: this.room.localParticipant.identity,
        trackId,
        codec,
      };
      // @ts-ignore
      sender.transform = new RTCRtpScriptTransform(this.worker, options);
    } else {
      log.info('initialize encoded streams');
      // @ts-ignore
      const senderStreams = sender.createEncodedStreams();
      const msg: EncodeMessage = {
        kind: 'encode',
        data: {
          readableStream: senderStreams.readable,
          writableStream: senderStreams.writable,
          codec,
          trackId,
          participantIdentity: this.room.localParticipant.identity,
          isReuse: false,
        },
      };
      this.worker.postMessage(msg, [senderStreams.readable, senderStreams.writable]);
    }

    // @ts-ignore
    sender[E2EE_FLAG] = true;
  }
}
