/* eslint-disable @typescript-eslint/no-unused-vars */
// TODO code inspired by https://github.com/webrtc/samples/blob/gh-pages/src/content/insertable-streams/endtoend-encryption/js/worker.js
import { EventEmitter } from 'events';
import type TypedEventEmitter from 'typed-emitter';
import { workerLogger } from '../../logger';
import type { VideoCodec } from '../../room/track/options';
import { ENCRYPTION_ALGORITHM, IV_LENGTH, UNENCRYPTED_BYTES } from '../constants';
import { CryptorError, CryptorErrorReason } from '../errors';
import { type CryptorCallbacks, CryptorEvent } from '../events';
import type { DecodeRatchetOptions, KeyProviderOptions, KeySet, RatchetResult } from '../types';
import { deriveKeys, isVideoFrame, needsRbspUnescaping, parseRbsp, writeRbsp } from '../utils';
import type { ParticipantKeyHandler } from './ParticipantKeyHandler';
import { processNALUsForEncryption } from './naluUtils';
import { identifySifPayload } from './sifPayload';

export const encryptionEnabledMap: Map<string, boolean> = new Map();

export interface FrameCryptorConstructor {
  new (opts?: unknown): BaseFrameCryptor;
}

export interface TransformerInfo {
  readable: ReadableStream;
  writable: WritableStream;
  transformer: TransformStream;
  abortController: AbortController;
}

export class BaseFrameCryptor extends (EventEmitter as new () => TypedEventEmitter<CryptorCallbacks>) {
  protected encodeFunction(
    encodedFrame: RTCEncodedVideoFrame | RTCEncodedAudioFrame,
    controller: TransformStreamDefaultController,
  ): Promise<any> {
    throw Error('not implemented for subclass');
  }

  protected decodeFunction(
    encodedFrame: RTCEncodedVideoFrame | RTCEncodedAudioFrame,
    controller: TransformStreamDefaultController,
  ): Promise<any> {
    throw Error('not implemented for subclass');
  }
}

/**
 * Cryptor is responsible for en-/decrypting media frames.
 * Each Cryptor instance is responsible for en-/decrypting a single mediaStreamTrack.
 */
export class FrameCryptor extends BaseFrameCryptor {
  private sendCounts: Map<number, number>;

  private participantIdentity: string | undefined;

  private trackId: string | undefined;

  private keys: ParticipantKeyHandler;

  private videoCodec?: VideoCodec;

  private rtpMap: Map<number, VideoCodec>;

  private keyProviderOptions: KeyProviderOptions;

  /**
   * used for detecting server injected unencrypted frames
   */
  private sifTrailer: Uint8Array;

  private detectedCodec?: VideoCodec;

  private isTransformActive: boolean = false;

  constructor(opts: {
    keys: ParticipantKeyHandler;
    participantIdentity: string;
    keyProviderOptions: KeyProviderOptions;
    sifTrailer?: Uint8Array;
  }) {
    super();
    this.sendCounts = new Map();
    this.keys = opts.keys;
    this.participantIdentity = opts.participantIdentity;
    this.rtpMap = new Map();
    this.keyProviderOptions = opts.keyProviderOptions;
    this.sifTrailer = opts.sifTrailer ?? Uint8Array.from([]);
  }

  private get logContext() {
    return {
      participant: this.participantIdentity,
      mediaTrackId: this.trackId,
      fallbackCodec: this.videoCodec,
    };
  }

  /**
   * Assign a different participant to the cryptor.
   * useful for transceiver re-use
   * @param id
   * @param keys
   */
  setParticipant(id: string, keys: ParticipantKeyHandler) {
    workerLogger.debug('setting new participant on cryptor', {
      ...this.logContext,
      participant: id,
    });
    if (this.participantIdentity) {
      workerLogger.error(
        'cryptor has already a participant set, participant should have been unset before',
        {
          ...this.logContext,
        },
      );
    }
    this.participantIdentity = id;
    this.keys = keys;
  }

  unsetParticipant() {
    workerLogger.debug('unsetting participant', this.logContext);
    this.participantIdentity = undefined;
  }

  isEnabled() {
    if (this.participantIdentity) {
      return encryptionEnabledMap.get(this.participantIdentity);
    } else {
      return undefined;
    }
  }

  getParticipantIdentity() {
    return this.participantIdentity;
  }

  getTrackId() {
    return this.trackId;
  }

  /**
   * Update the video codec used by the mediaStreamTrack
   * @param codec
   */
  setVideoCodec(codec: VideoCodec) {
    this.videoCodec = codec;
  }

  /**
   * rtp payload type map used for figuring out codec of payload type when encoding
   * @param map
   */
  setRtpMap(map: Map<number, VideoCodec>) {
    this.rtpMap = map;
  }

  setupTransform(
    operation: 'encode' | 'decode',
    readable: ReadableStream<RTCEncodedVideoFrame | RTCEncodedAudioFrame>,
    writable: WritableStream<RTCEncodedVideoFrame | RTCEncodedAudioFrame>,
    trackId: string,
    isReuse: boolean,
    codec?: VideoCodec,
  ) {
    if (codec) {
      workerLogger.info('setting codec on cryptor to', { codec });
      this.videoCodec = codec;
    }

    workerLogger.debug('Setting up frame cryptor transform', {
      operation,
      passedTrackId: trackId,
      codec,
      ...this.logContext,
    });

    if (isReuse && this.isTransformActive) {
      workerLogger.debug('reuse transform', {
        ...this.logContext,
      });
      return;
    }

    const transformFn = operation === 'encode' ? this.encodeFunction : this.decodeFunction;
    const transformStream = new TransformStream({
      transform: transformFn.bind(this),
    });

    this.isTransformActive = true;

    readable
      .pipeThrough(transformStream)
      .pipeTo(writable)
      .catch((e) => {
        workerLogger.warn(e);
        this.emit(
          CryptorEvent.Error,
          e instanceof CryptorError
            ? e
            : new CryptorError(e.message, undefined, this.participantIdentity),
        );
      })
      .finally(() => {
        this.isTransformActive = false;
      });
    this.trackId = trackId;
  }

  setSifTrailer(trailer: Uint8Array) {
    workerLogger.debug('setting SIF trailer', { ...this.logContext, trailer });
    this.sifTrailer = trailer;
  }

  /**
   * Function that will be injected in a stream and will encrypt the given encoded frames.
   *
   * @param {RTCEncodedVideoFrame|RTCEncodedAudioFrame} encodedFrame - Encoded video frame.
   * @param {TransformStreamDefaultController} controller - TransportStreamController.
   *
   * The VP8 payload descriptor described in
   * https://tools.ietf.org/html/rfc7741#section-4.2
   * is part of the RTP packet and not part of the frame and is not controllable by us.
   * This is fine as the SFU keeps having access to it for routing.
   *
   * The encrypted frame is formed as follows:
   * 1) Find unencrypted byte length, depending on the codec, frame type and kind.
   * 2) Form the GCM IV for the frame as described above.
   * 3) Encrypt the rest of the frame using AES-GCM.
   * 4) Allocate space for the encrypted frame.
   * 5) Copy the unencrypted bytes to the start of the encrypted frame.
   * 6) Append the ciphertext to the encrypted frame.
   * 7) Append the IV.
   * 8) Append a single byte for the key identifier.
   * 9) Enqueue the encrypted frame for sending.
   */
  protected async encodeFunction(
    encodedFrame: RTCEncodedVideoFrame | RTCEncodedAudioFrame,
    controller: TransformStreamDefaultController,
  ) {
    if (
      !this.isEnabled() ||
      // skip for encryption for empty dtx frames
      encodedFrame.data.byteLength === 0
    ) {
      return controller.enqueue(encodedFrame);
    }
    const keySet = this.keys.getKeySet();
    if (!keySet) {
      this.emit(
        CryptorEvent.Error,
        new CryptorError(
          `key set not found for ${
            this.participantIdentity
          } at index ${this.keys.getCurrentKeyIndex()}`,
          CryptorErrorReason.MissingKey,
          this.participantIdentity,
        ),
      );
      return;
    }
    const { encryptionKey } = keySet;
    const keyIndex = this.keys.getCurrentKeyIndex();

    if (encryptionKey) {
      const iv = this.makeIV(
        encodedFrame.getMetadata().synchronizationSource ?? -1,
        encodedFrame.timestamp,
      );
      let frameInfo = this.getUnencryptedBytes(encodedFrame);

      // Thіs is not encrypted and contains the VP8 payload descriptor or the Opus TOC byte.
      const frameHeader = new Uint8Array(encodedFrame.data, 0, frameInfo.unencryptedBytes);

      // Frame trailer contains the R|IV_LENGTH and key index
      const frameTrailer = new Uint8Array(2);

      frameTrailer[0] = IV_LENGTH;
      frameTrailer[1] = keyIndex;

      // Construct frame trailer. Similar to the frame header described in
      // https://tools.ietf.org/html/draft-omara-sframe-00#section-4.2
      // but we put it at the end.
      //
      // ---------+-------------------------+-+---------+----
      // payload  |IV...(length = IV_LENGTH)|R|IV_LENGTH|KID |
      // ---------+-------------------------+-+---------+----
      try {
        const cipherText = await crypto.subtle.encrypt(
          {
            name: ENCRYPTION_ALGORITHM,
            iv,
            additionalData: new Uint8Array(encodedFrame.data, 0, frameHeader.byteLength),
          },
          encryptionKey,
          new Uint8Array(encodedFrame.data, frameInfo.unencryptedBytes),
        );

        let newDataWithoutHeader = new Uint8Array(
          cipherText.byteLength + iv.byteLength + frameTrailer.byteLength,
        );
        newDataWithoutHeader.set(new Uint8Array(cipherText)); // add ciphertext.
        newDataWithoutHeader.set(new Uint8Array(iv), cipherText.byteLength); // append IV.
        newDataWithoutHeader.set(frameTrailer, cipherText.byteLength + iv.byteLength); // append frame trailer.

        if (frameInfo.requiresNALUProcessing) {
          newDataWithoutHeader = writeRbsp(newDataWithoutHeader);
        }

        var newData = new Uint8Array(frameHeader.byteLength + newDataWithoutHeader.byteLength);
        newData.set(frameHeader);
        newData.set(newDataWithoutHeader, frameHeader.byteLength);

        encodedFrame.data = newData.buffer;

        return controller.enqueue(encodedFrame);
      } catch (e: any) {
        // TODO: surface this to the app.
        workerLogger.error(e);
      }
    } else {
      workerLogger.debug('failed to encrypt, emitting error', this.logContext);
      this.emit(
        CryptorEvent.Error,
        new CryptorError(
          `encryption key missing for encoding`,
          CryptorErrorReason.MissingKey,
          this.participantIdentity,
        ),
      );
    }
  }

  /**
   * Function that will be injected in a stream and will decrypt the given encoded frames.
   *
   * @param {RTCEncodedVideoFrame|RTCEncodedAudioFrame} encodedFrame - Encoded video frame.
   * @param {TransformStreamDefaultController} controller - TransportStreamController.
   */
  protected async decodeFunction(
    encodedFrame: RTCEncodedVideoFrame | RTCEncodedAudioFrame,
    controller: TransformStreamDefaultController,
  ) {
    if (
      !this.isEnabled() ||
      // skip for decryption for empty dtx frames
      encodedFrame.data.byteLength === 0
    ) {
      return controller.enqueue(encodedFrame);
    }

    if (isFrameServerInjected(encodedFrame.data, this.sifTrailer)) {
      encodedFrame.data = encodedFrame.data.slice(
        0,
        encodedFrame.data.byteLength - this.sifTrailer.byteLength,
      );
      if (await identifySifPayload(encodedFrame.data)) {
        workerLogger.debug('enqueue SIF', this.logContext);
        return controller.enqueue(encodedFrame);
      } else {
        workerLogger.warn('Unexpected SIF frame payload, dropping frame', this.logContext);
        return;
      }
    }
    const data = new Uint8Array(encodedFrame.data);
    const keyIndex = data[encodedFrame.data.byteLength - 1];

    if (this.keys.hasInvalidKeyAtIndex(keyIndex)) {
      // drop frame
      return;
    }

    if (this.keys.getKeySet(keyIndex)) {
      try {
        const decodedFrame = await this.decryptFrame(encodedFrame, keyIndex);
        this.keys.decryptionSuccess(keyIndex);
        if (decodedFrame) {
          return controller.enqueue(decodedFrame);
        }
      } catch (error) {
        if (error instanceof CryptorError && error.reason === CryptorErrorReason.InvalidKey) {
          // emit an error if the key handler thinks we have a valid key
          if (this.keys.hasValidKey) {
            this.emit(CryptorEvent.Error, error);
            this.keys.decryptionFailure(keyIndex);
          }
        } else {
          workerLogger.warn('decoding frame failed', { error });
        }
      }
    } else {
      // emit an error if the key index is out of bounds but the key handler thinks we still have a valid key
      workerLogger.warn(`skipping decryption due to missing key at index ${keyIndex}`);
      this.emit(
        CryptorEvent.Error,
        new CryptorError(
          `missing key at index ${keyIndex} for participant ${this.participantIdentity}`,
          CryptorErrorReason.MissingKey,
          this.participantIdentity,
        ),
      );
      this.keys.decryptionFailure(keyIndex);
    }
  }

  /**
   * Function that will decrypt the given encoded frame. If the decryption fails, it will
   * ratchet the key for up to RATCHET_WINDOW_SIZE times.
   */
  private async decryptFrame(
    encodedFrame: RTCEncodedVideoFrame | RTCEncodedAudioFrame,
    keyIndex: number,
    initialMaterial: KeySet | undefined = undefined,
    ratchetOpts: DecodeRatchetOptions = { ratchetCount: 0 },
  ): Promise<RTCEncodedVideoFrame | RTCEncodedAudioFrame | undefined> {
    const keySet = this.keys.getKeySet(keyIndex);
    if (!ratchetOpts.encryptionKey && !keySet) {
      throw new TypeError(`no encryption key found for decryption of ${this.participantIdentity}`);
    }
    let frameInfo = this.getUnencryptedBytes(encodedFrame);

    // Construct frame trailer. Similar to the frame header described in
    // https://tools.ietf.org/html/draft-omara-sframe-00#section-4.2
    // but we put it at the end.
    //
    // ---------+-------------------------+-+---------+----
    // payload  |IV...(length = IV_LENGTH)|R|IV_LENGTH|KID |
    // ---------+-------------------------+-+---------+----

    try {
      const frameHeader = new Uint8Array(encodedFrame.data, 0, frameInfo.unencryptedBytes);
      var encryptedData = new Uint8Array(
        encodedFrame.data,
        frameHeader.length,
        encodedFrame.data.byteLength - frameHeader.length,
      );
      if (frameInfo.requiresNALUProcessing && needsRbspUnescaping(encryptedData)) {
        encryptedData = parseRbsp(encryptedData);
        const newUint8 = new Uint8Array(frameHeader.byteLength + encryptedData.byteLength);
        newUint8.set(frameHeader);
        newUint8.set(encryptedData, frameHeader.byteLength);
        encodedFrame.data = newUint8.buffer;
      }

      const frameTrailer = new Uint8Array(encodedFrame.data, encodedFrame.data.byteLength - 2, 2);

      const ivLength = frameTrailer[0];
      const iv = new Uint8Array(
        encodedFrame.data,
        encodedFrame.data.byteLength - ivLength - frameTrailer.byteLength,
        ivLength,
      );

      const cipherTextStart = frameHeader.byteLength;
      const cipherTextLength =
        encodedFrame.data.byteLength -
        (frameHeader.byteLength + ivLength + frameTrailer.byteLength);

      const plainText = await crypto.subtle.decrypt(
        {
          name: ENCRYPTION_ALGORITHM,
          iv,
          additionalData: new Uint8Array(encodedFrame.data, 0, frameHeader.byteLength),
        },
        ratchetOpts.encryptionKey ?? keySet!.encryptionKey,
        new Uint8Array(encodedFrame.data, cipherTextStart, cipherTextLength),
      );

      const newData = new ArrayBuffer(frameHeader.byteLength + plainText.byteLength);
      const newUint8 = new Uint8Array(newData);

      newUint8.set(new Uint8Array(encodedFrame.data, 0, frameHeader.byteLength));
      newUint8.set(new Uint8Array(plainText), frameHeader.byteLength);

      encodedFrame.data = newData;

      return encodedFrame;
    } catch (error: any) {
      if (this.keyProviderOptions.ratchetWindowSize > 0) {
        if (ratchetOpts.ratchetCount < this.keyProviderOptions.ratchetWindowSize) {
          workerLogger.debug(
            `ratcheting key attempt ${ratchetOpts.ratchetCount} of ${
              this.keyProviderOptions.ratchetWindowSize
            }, for kind ${encodedFrame instanceof RTCEncodedAudioFrame ? 'audio' : 'video'}`,
          );

          let ratchetedKeySet: KeySet | undefined;
          let ratchetResult: RatchetResult | undefined;
          if ((initialMaterial ?? keySet) === this.keys.getKeySet(keyIndex)) {
            // only ratchet if the currently set key is still the same as the one used to decrypt this frame
            // if not, it might be that a different frame has already ratcheted and we try with that one first
            ratchetResult = await this.keys.ratchetKey(keyIndex, false);

            ratchetedKeySet = await deriveKeys(
              ratchetResult.cryptoKey,
              this.keyProviderOptions.ratchetSalt,
            );
          }

          const frame = await this.decryptFrame(encodedFrame, keyIndex, initialMaterial || keySet, {
            ratchetCount: ratchetOpts.ratchetCount + 1,
            encryptionKey: ratchetedKeySet?.encryptionKey,
          });
          if (frame && ratchetedKeySet) {
            // before updating the keys, make sure that the keySet used for this frame is still the same as the currently set key
            // if it's not, a new key might have been set already, which we don't want to override
            if ((initialMaterial ?? keySet) === this.keys.getKeySet(keyIndex)) {
              this.keys.setKeySet(ratchetedKeySet, keyIndex, ratchetResult);
              // decryption was successful, set the new key index to reflect the ratcheted key set
              this.keys.setCurrentKeyIndex(keyIndex);
            }
          }
          return frame;
        } else {
          /**
           * Because we only set a new key once decryption has been successful,
           * we can be sure that we don't need to reset the key to the initial material at this point
           * as the key has not been updated on the keyHandler instance
           */

          workerLogger.warn('maximum ratchet attempts exceeded');
          throw new CryptorError(
            `valid key missing for participant ${this.participantIdentity}`,
            CryptorErrorReason.InvalidKey,
            this.participantIdentity,
          );
        }
      } else {
        throw new CryptorError(
          `Decryption failed: ${error.message}`,
          CryptorErrorReason.InvalidKey,
          this.participantIdentity,
        );
      }
    }
  }

  /**
   * Construct the IV used for AES-GCM and sent (in plain) with the packet similar to
   * https://tools.ietf.org/html/rfc7714#section-8.1
   * It concatenates
   * - the 32 bit synchronization source (SSRC) given on the encoded frame,
   * - the 32 bit rtp timestamp given on the encoded frame,
   * - a send counter that is specific to the SSRC. Starts at a random number.
   * The send counter is essentially the pictureId but we currently have to implement this ourselves.
   * There is no XOR with a salt. Note that this IV leaks the SSRC to the receiver but since this is
   * randomly generated and SFUs may not rewrite this is considered acceptable.
   * The SSRC is used to allow demultiplexing multiple streams with the same key, as described in
   *   https://tools.ietf.org/html/rfc3711#section-4.1.1
   * The RTP timestamp is 32 bits and advances by the codec clock rate (90khz for video, 48khz for
   * opus audio) every second. For video it rolls over roughly every 13 hours.
   * The send counter will advance at the frame rate (30fps for video, 50fps for 20ms opus audio)
   * every second. It will take a long time to roll over.
   *
   * See also https://developer.mozilla.org/en-US/docs/Web/API/AesGcmParams
   */
  private makeIV(synchronizationSource: number, timestamp: number) {
    const iv = new ArrayBuffer(IV_LENGTH);
    const ivView = new DataView(iv);

    // having to keep our own send count (similar to a picture id) is not ideal.
    if (!this.sendCounts.has(synchronizationSource)) {
      // Initialize with a random offset, similar to the RTP sequence number.
      this.sendCounts.set(synchronizationSource, Math.floor(Math.random() * 0xffff));
    }

    const sendCount = this.sendCounts.get(synchronizationSource) ?? 0;

    ivView.setUint32(0, synchronizationSource);
    ivView.setUint32(4, timestamp);
    ivView.setUint32(8, timestamp - (sendCount % 0xffff));

    this.sendCounts.set(synchronizationSource, sendCount + 1);

    return iv;
  }

  private getUnencryptedBytes(frame: RTCEncodedVideoFrame | RTCEncodedAudioFrame): {
    unencryptedBytes: number;
    requiresNALUProcessing: boolean;
  } {
    // Handle audio frames
    if (!isVideoFrame(frame)) {
      return { unencryptedBytes: UNENCRYPTED_BYTES.audio, requiresNALUProcessing: false };
    }

    // Detect and track codec changes
    const detectedCodec = this.getVideoCodec(frame) ?? this.videoCodec;
    if (detectedCodec !== this.detectedCodec) {
      workerLogger.debug('detected different codec', {
        detectedCodec,
        oldCodec: this.detectedCodec,
        ...this.logContext,
      });
      this.detectedCodec = detectedCodec;
    }

    // Check for unsupported codecs
    if (detectedCodec === 'av1') {
      throw new Error(`${detectedCodec} is not yet supported for end to end encryption`);
    }

    // Handle VP8/VP9 codecs (no NALU processing needed)
    if (detectedCodec === 'vp8') {
      return { unencryptedBytes: UNENCRYPTED_BYTES[frame.type], requiresNALUProcessing: false };
    }
    if (detectedCodec === 'vp9') {
      return { unencryptedBytes: 0, requiresNALUProcessing: false };
    }

    // Try NALU processing for H.264/H.265 codecs
    try {
      const knownCodec =
        detectedCodec === 'h264' || detectedCodec === 'h265' ? detectedCodec : undefined;
      const naluResult = processNALUsForEncryption(new Uint8Array(frame.data), knownCodec);

      if (naluResult.requiresNALUProcessing) {
        return {
          unencryptedBytes: naluResult.unencryptedBytes,
          requiresNALUProcessing: true,
        };
      }
    } catch (e) {
      workerLogger.debug('NALU processing failed, falling back to VP8 handling', {
        error: e,
        ...this.logContext,
      });
    }

    // Fallback to VP8 handling
    return { unencryptedBytes: UNENCRYPTED_BYTES[frame.type], requiresNALUProcessing: false };
  }

  /**
   * inspects frame payloadtype if available and maps it to the codec specified in rtpMap
   */
  private getVideoCodec(frame: RTCEncodedVideoFrame): VideoCodec | undefined {
    if (this.rtpMap.size === 0) {
      return undefined;
    }
    const payloadType = frame.getMetadata().payloadType;
    const codec = payloadType ? this.rtpMap.get(payloadType) : undefined;
    return codec;
  }
}

/**
 * we use a magic frame trailer to detect whether a frame is injected
 * by the livekit server and thus to be treated as unencrypted
 * @internal
 */
export function isFrameServerInjected(frameData: ArrayBuffer, trailerBytes: Uint8Array): boolean {
  if (trailerBytes.byteLength === 0) {
    return false;
  }
  const frameTrailer = new Uint8Array(
    frameData.slice(frameData.byteLength - trailerBytes.byteLength),
  );
  return trailerBytes.every((value, index) => value === frameTrailer[index]);
}
