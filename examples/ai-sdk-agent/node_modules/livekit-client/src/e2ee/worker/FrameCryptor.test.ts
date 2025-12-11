import { afterEach, describe, expect, it, vitest } from 'vitest';
import { IV_LENGTH, KEY_PROVIDER_DEFAULTS } from '../constants';
import { CryptorEvent } from '../events';
import type { KeyProviderOptions } from '../types';
import { createKeyMaterialFromString } from '../utils';
import { FrameCryptor, encryptionEnabledMap, isFrameServerInjected } from './FrameCryptor';
import { ParticipantKeyHandler } from './ParticipantKeyHandler';

function mockEncryptedRTCEncodedVideoFrame(keyIndex: number): RTCEncodedVideoFrame {
  const trailer = mockFrameTrailer(keyIndex);
  const data = new Uint8Array(trailer.length + 10);
  data.set(trailer, 10);
  return mockRTCEncodedVideoFrame(data);
}

function mockRTCEncodedVideoFrame(data: Uint8Array): RTCEncodedVideoFrame {
  return {
    data: data.buffer,
    timestamp: vitest.getMockedSystemTime()?.getTime() ?? 0,
    type: 'key',
    getMetadata(): RTCEncodedVideoFrameMetadata {
      return {};
    },
  };
}

function mockFrameTrailer(keyIndex: number): Uint8Array {
  const frameTrailer = new Uint8Array(2);

  frameTrailer[0] = IV_LENGTH;
  frameTrailer[1] = keyIndex;

  return frameTrailer;
}

class TestUnderlyingSource<T> implements UnderlyingSource<T> {
  controller: ReadableStreamController<T>;

  start(controller: ReadableStreamController<T>): void {
    this.controller = controller;
  }

  write(chunk: T): void {
    this.controller.enqueue(chunk as any);
  }

  close(): void {
    this.controller.close();
  }
}

class TestUnderlyingSink<T> implements UnderlyingSink<T> {
  public chunks: T[] = [];

  write(chunk: T): void {
    this.chunks.push(chunk);
  }
}

function prepareParticipantTestDecoder(
  participantIdentity: string,
  partialKeyProviderOptions: Partial<KeyProviderOptions>,
) {
  return prepareParticipantTest('decode', participantIdentity, partialKeyProviderOptions);
}

function prepareParticipantTestEncoder(
  participantIdentity: string,
  partialKeyProviderOptions: Partial<KeyProviderOptions>,
) {
  return prepareParticipantTest('encode', participantIdentity, partialKeyProviderOptions);
}

function prepareParticipantTest(
  mode: 'encode' | 'decode',
  participantIdentity: string,
  partialKeyProviderOptions: Partial<KeyProviderOptions>,
): {
  keys: ParticipantKeyHandler;
  cryptor: FrameCryptor;
  input: TestUnderlyingSource<RTCEncodedVideoFrame>;
  output: TestUnderlyingSink<RTCEncodedVideoFrame>;
} {
  const keyProviderOptions = { ...KEY_PROVIDER_DEFAULTS, ...partialKeyProviderOptions };
  const keys = new ParticipantKeyHandler(participantIdentity, keyProviderOptions);

  encryptionEnabledMap.set(participantIdentity, true);

  const cryptor = new FrameCryptor({
    participantIdentity,
    keys,
    keyProviderOptions,
    sifTrailer: new Uint8Array(),
  });

  const input = new TestUnderlyingSource<RTCEncodedVideoFrame>();
  const output = new TestUnderlyingSink<RTCEncodedVideoFrame>();
  cryptor.setupTransform(mode, new ReadableStream(input), new WritableStream(output), 'testTrack');

  return { keys, cryptor, input, output };
}

describe('FrameCryptor', () => {
  const participantIdentity = 'testParticipant';

  it('identifies server injected frame correctly', () => {
    const frameTrailer = new TextEncoder().encode('LKROCKS');
    const frameData = new Uint8Array([1, 2, 3, 4, 5, 6, 7, 8, ...frameTrailer]).buffer;

    expect(isFrameServerInjected(frameData, frameTrailer)).toBe(true);
  });

  it('identifies server non server injected frame correctly', () => {
    const frameTrailer = new TextEncoder().encode('LKROCKS');
    const frameData = new Uint8Array([1, 2, 3, 4, 5, 6, 7, 8, ...frameTrailer, 10]);

    expect(isFrameServerInjected(frameData.buffer, frameTrailer)).toBe(false);
    frameData.fill(0);
    expect(isFrameServerInjected(frameData.buffer, frameTrailer)).toBe(false);
  });

  describe('encode', () => {
    afterEach(() => {
      encryptionEnabledMap.clear();
    });

    it('passthrough if participant encryption disabled', async () => {
      vitest.useFakeTimers();
      try {
        const { input, output } = prepareParticipantTestEncoder(participantIdentity, {});

        // disable encryption for participant
        encryptionEnabledMap.set(participantIdentity, false);

        const frame = mockRTCEncodedVideoFrame(new Uint8Array([1, 2, 3, 4, 5, 6, 7, 8]));

        input.write(frame);
        await vitest.advanceTimersToNextTimerAsync();

        expect(output.chunks).toEqual([frame]);
      } finally {
        vitest.useRealTimers();
      }
    });

    it('passthrough for empty frame', async () => {
      vitest.useFakeTimers();
      try {
        const { input, output } = prepareParticipantTestEncoder(participantIdentity, {});

        // empty frame
        const frame = mockRTCEncodedVideoFrame(new Uint8Array(0));

        input.write(frame);
        await vitest.advanceTimersToNextTimerAsync();

        expect(output.chunks).toEqual([frame]);
      } finally {
        vitest.useRealTimers();
      }
    });

    it('immediately drops frame and emits error if no key set', async () => {
      vitest.useFakeTimers();
      try {
        const { cryptor, input, output } = prepareParticipantTestEncoder(participantIdentity, {});

        const errorListener = vitest.fn();
        cryptor.on(CryptorEvent.Error, errorListener);

        const frame = mockRTCEncodedVideoFrame(new Uint8Array([1, 2, 3, 4, 5, 6, 7, 8]));

        input.write(frame);
        await vitest.advanceTimersToNextTimerAsync();

        expect(output.chunks).toEqual([]);
        expect(errorListener).toHaveBeenCalled();
      } finally {
        vitest.useRealTimers();
      }
    });

    it('encrypts frame', async () => {
      vitest.useFakeTimers();
      try {
        const { keys, input, output } = prepareParticipantTestEncoder(participantIdentity, {});

        await keys.setKey(await createKeyMaterialFromString('key1'), 1);

        const plainTextData = new Uint8Array([
          1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16,
        ]);
        const frame = mockRTCEncodedVideoFrame(plainTextData);

        input.write(frame);
        await vitest.waitFor(() => expect(output.chunks).toHaveLength(1));

        expect(output.chunks).toEqual([frame]);
        expect(frame.data.byteLength).toBeGreaterThan(16);

        // first bytes are unencrypted
        expect(new Uint8Array(frame.data.slice(0, 10))).toEqual(plainTextData.subarray(0, 10));

        // remainder should not be the same
        expect(new Uint8Array(frame.data.slice(10, 16))).not.toEqual(
          plainTextData.subarray(10, 16),
        );

        const frameTrailer = new Uint8Array(frame.data.slice(frame.data.byteLength - 2));
        // IV length
        expect(frameTrailer[0]).toEqual(IV_LENGTH);
        // key index
        expect(frameTrailer[1]).toEqual(1);
      } finally {
        vitest.useRealTimers();
      }
    });
  });

  describe('decode', () => {
    afterEach(() => {
      encryptionEnabledMap.clear();
    });

    it('passthrough if participant encryption disabled', async () => {
      vitest.useFakeTimers();
      try {
        const { input, output } = prepareParticipantTestDecoder(participantIdentity, {});

        // disable encryption for participant
        encryptionEnabledMap.set(participantIdentity, false);

        const frame = mockEncryptedRTCEncodedVideoFrame(1);

        input.write(frame);
        await vitest.advanceTimersToNextTimerAsync();

        expect(output.chunks).toEqual([frame]);
      } finally {
        vitest.useRealTimers();
      }
    });

    it('passthrough for empty frame', async () => {
      vitest.useFakeTimers();
      try {
        const { input, output } = prepareParticipantTestDecoder(participantIdentity, {});

        // empty frame
        const frame = mockRTCEncodedVideoFrame(new Uint8Array(0));

        input.write(frame);
        await vitest.advanceTimersToNextTimerAsync();

        expect(output.chunks).toEqual([frame]);
      } finally {
        vitest.useRealTimers();
      }
    });

    it('immediately drops frames when key marked invalid', async () => {
      vitest.useFakeTimers();
      try {
        const { keys, input, output } = prepareParticipantTestDecoder(participantIdentity, {
          failureTolerance: 0,
        });

        keys.decryptionFailure();

        input.write(mockEncryptedRTCEncodedVideoFrame(1));
        await vitest.advanceTimersToNextTimerAsync();

        expect(output.chunks).toEqual([]);

        keys.decryptionFailure();

        input.write(mockEncryptedRTCEncodedVideoFrame(0));
        await vitest.advanceTimersToNextTimerAsync();

        expect(output.chunks).toEqual([]);
      } finally {
        vitest.useRealTimers();
      }
    });

    it('calls decryptionFailure on missing key and emits error', async () => {
      vitest.useFakeTimers();
      try {
        const { cryptor, keys, input } = prepareParticipantTestDecoder(participantIdentity, {});

        const errorListener = vitest.fn();
        cryptor.on(CryptorEvent.Error, errorListener);
        vitest.spyOn(keys, 'decryptionFailure');

        // no key is set at this index
        input.write(mockEncryptedRTCEncodedVideoFrame(1));
        await vitest.advanceTimersToNextTimerAsync();

        expect(keys.decryptionFailure).toHaveBeenCalledTimes(1);
        expect(keys.decryptionFailure).toHaveBeenCalledWith(1);
        expect(errorListener).toHaveBeenCalled();
      } finally {
        vitest.useRealTimers();
      }
    });

    it('immediately drops frame if no key', async () => {
      vitest.useFakeTimers();
      try {
        const { input, output } = prepareParticipantTestDecoder(participantIdentity, {});

        vitest.spyOn(crypto.subtle, 'decrypt');

        input.write(mockEncryptedRTCEncodedVideoFrame(1));
        await vitest.advanceTimersToNextTimerAsync();

        expect(crypto.subtle.decrypt).not.toHaveBeenCalled();
        expect(output.chunks).toEqual([]);
      } finally {
        vitest.useRealTimers();
      }
    });

    it('calls decryptionFailure with incorrect key and emits error', async () => {
      vitest.useFakeTimers();
      try {
        const { cryptor, keys, input, output } = prepareParticipantTestDecoder(
          participantIdentity,
          { ratchetWindowSize: 0 },
        );

        vitest.spyOn(crypto.subtle, 'decrypt');
        vitest.spyOn(keys, 'decryptionFailure');
        const errorListener = vitest.fn();
        cryptor.on(CryptorEvent.Error, errorListener);

        await keys.setKey(await createKeyMaterialFromString('incorrect key'), 1);

        const frame = mockRTCEncodedVideoFrame(
          new Uint8Array([
            1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 254, 96, 91, 111, 187, 132, 31, 12, 207, 136, 17, 221,
            233, 116, 174, 6, 50, 37, 214, 71, 119, 196, 255, 255, 255, 255, 0, 0, 0, 0, 255, 255,
            199, 51, 12, 1,
          ]),
        );
        // global.RTCEncodedAudioFrame = vitest.fn();
        input.write(frame);
        await vitest.waitFor(() => expect(keys.decryptionFailure).toHaveBeenCalled());

        expect(crypto.subtle.decrypt).toHaveBeenCalled();
        expect(output.chunks).toEqual([]);
        expect(errorListener).toHaveBeenCalled();
        expect(keys.decryptionFailure).toHaveBeenCalledTimes(1);
        expect(keys.decryptionFailure).toHaveBeenCalledWith(1);
      } finally {
        vitest.useRealTimers();
      }
    });

    it('decrypts frame with correct key', async () => {
      vitest.useFakeTimers();
      try {
        const { keys, input, output } = prepareParticipantTestDecoder(participantIdentity, {});

        vitest.spyOn(keys, 'decryptionSuccess');

        await keys.setKey(await createKeyMaterialFromString('key1'), 1);

        const frame = mockRTCEncodedVideoFrame(
          new Uint8Array([
            1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 254, 96, 91, 111, 187, 132, 31, 12, 207, 136, 17, 221,
            233, 116, 174, 6, 50, 37, 214, 71, 119, 196, 255, 255, 255, 255, 0, 0, 0, 0, 255, 255,
            199, 51, 12, 1,
          ]),
        );
        input.write(frame);
        await vitest.waitFor(() => expect(output.chunks).toHaveLength(1));

        expect(output.chunks).toEqual([frame]);

        expect(keys.decryptionSuccess).toHaveBeenCalledTimes(1);
        expect(keys.decryptionSuccess).toHaveBeenCalledWith(1);

        expect(frame.data.byteLength).toBe(16);

        expect(new Uint8Array(frame.data)).toEqual(
          new Uint8Array([1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16]),
        );
      } finally {
        vitest.useRealTimers();
      }
    });

    it('recovers from delayed use of rotated key', async () => {
      vitest.useFakeTimers();
      try {
        // 1. we (the local participant) have just joined a room and do not have the existing key (index 0) for the existing/remote participant
        const { keys, input, output } = prepareParticipantTestDecoder(participantIdentity, {
          failureTolerance: 1,
          ratchetWindowSize: 0,
        });
        vitest.spyOn(keys, 'decryptionFailure');

        // 2. we receive some frames from the existing participant encrypted with the existing key 0 that we don't have
        input.write(mockEncryptedRTCEncodedVideoFrame(0));
        input.write(mockEncryptedRTCEncodedVideoFrame(0));
        input.write(mockEncryptedRTCEncodedVideoFrame(0));
        input.write(mockEncryptedRTCEncodedVideoFrame(0));

        // 3. we should have marked key at index 0 as invalid by now and dropped all the frames
        await vitest.waitFor(() => expect(keys.decryptionFailure).toHaveBeenCalledTimes(2));
        expect(keys.hasInvalidKeyAtIndex(0)).toBe(true);
        expect(output.chunks).toEqual([]);

        // 4. the existing participant then notices that we have joined the room and generates a new key (with a new key index 1)
        // and distributes it out of band to us
        await keys.setKey(await createKeyMaterialFromString('key1'), 1);

        // 5. the existing participant waits a period of time before using the new key and continues sending media using the previous key 0.
        // we receive these frames and should drop them as we still don't have the key.
        input.write(mockEncryptedRTCEncodedVideoFrame(0));
        input.write(mockEncryptedRTCEncodedVideoFrame(0));
        input.write(mockEncryptedRTCEncodedVideoFrame(0));
        input.write(mockEncryptedRTCEncodedVideoFrame(0));

        await vitest.advanceTimersToNextTimerAsync();
        expect(output.chunks).toEqual([]);

        // 6. the existing participant moves over to the new key index 1 and we start to receive frames for index 1 that we
        // should be able to decrypt even though we had the previous failures.
        input.write(
          mockRTCEncodedVideoFrame(
            new Uint8Array([
              1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 254, 96, 91, 111, 187, 132, 31, 12, 207, 136, 17, 221,
              233, 116, 174, 6, 50, 37, 214, 71, 119, 196, 255, 255, 255, 255, 0, 0, 0, 0, 255, 255,
              199, 51, 12, 1,
            ]),
          ),
        );

        input.write(
          mockRTCEncodedVideoFrame(
            new Uint8Array([
              99, 2, 3, 4, 5, 6, 7, 8, 9, 10, 154, 108, 209, 239, 253, 33, 72, 111, 13, 125, 10,
              101, 28, 209, 141, 162, 0, 238, 189, 254, 66, 156, 255, 255, 255, 255, 0, 0, 0, 0,
              255, 255, 96, 247, 12, 1,
            ]),
          ),
        );

        await vitest.waitFor(() => expect(output.chunks.length).toEqual(2));

        expect(new Uint8Array(output.chunks[0].data)).toEqual(
          new Uint8Array([1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16]),
        );
        expect(new Uint8Array(output.chunks[1].data)).toEqual(
          new Uint8Array([99, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16]),
        );
      } finally {
        vitest.useRealTimers();
      }
    });
  });
});
