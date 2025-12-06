import { type DataPacket, EncryptedPacketPayload } from '@livekit/protocol';
import { ENCRYPTION_ALGORITHM } from './constants';

export function isE2EESupported() {
  return isInsertableStreamSupported() || isScriptTransformSupported();
}

export function isScriptTransformSupported() {
  // @ts-ignore
  return typeof window.RTCRtpScriptTransform !== 'undefined';
}

export function isInsertableStreamSupported() {
  return (
    typeof window.RTCRtpSender !== 'undefined' &&
    // @ts-ignore
    typeof window.RTCRtpSender.prototype.createEncodedStreams !== 'undefined'
  );
}

export function isVideoFrame(
  frame: RTCEncodedAudioFrame | RTCEncodedVideoFrame,
): frame is RTCEncodedVideoFrame {
  return 'type' in frame;
}

export async function importKey(
  keyBytes: Uint8Array | ArrayBuffer,
  algorithm: string | { name: string } = { name: ENCRYPTION_ALGORITHM },
  usage: 'derive' | 'encrypt' = 'encrypt',
) {
  // https://developer.mozilla.org/en-US/docs/Web/API/SubtleCrypto/importKey
  return crypto.subtle.importKey(
    'raw',
    keyBytes,
    algorithm,
    false,
    usage === 'derive' ? ['deriveBits', 'deriveKey'] : ['encrypt', 'decrypt'],
  );
}

export async function createKeyMaterialFromString(password: string) {
  let enc = new TextEncoder();

  const keyMaterial = await crypto.subtle.importKey(
    'raw',
    enc.encode(password),
    {
      name: 'PBKDF2',
    },
    false,
    ['deriveBits', 'deriveKey'],
  );

  return keyMaterial;
}

export async function createKeyMaterialFromBuffer(cryptoBuffer: ArrayBuffer) {
  const keyMaterial = await crypto.subtle.importKey('raw', cryptoBuffer, 'HKDF', false, [
    'deriveBits',
    'deriveKey',
  ]);

  return keyMaterial;
}

function getAlgoOptions(algorithmName: string, salt: string) {
  const textEncoder = new TextEncoder();
  const encodedSalt = textEncoder.encode(salt);
  switch (algorithmName) {
    case 'HKDF':
      return {
        name: 'HKDF',
        salt: encodedSalt,
        hash: 'SHA-256',
        info: new ArrayBuffer(128),
      };
    case 'PBKDF2': {
      return {
        name: 'PBKDF2',
        salt: encodedSalt,
        hash: 'SHA-256',
        iterations: 100000,
      };
    }
    default:
      throw new Error(`algorithm ${algorithmName} is currently unsupported`);
  }
}

/**
 * Derives a set of keys from the master key.
 * See https://tools.ietf.org/html/draft-omara-sframe-00#section-4.3.1
 */
export async function deriveKeys(material: CryptoKey, salt: string) {
  const algorithmOptions = getAlgoOptions(material.algorithm.name, salt);

  // https://developer.mozilla.org/en-US/docs/Web/API/SubtleCrypto/deriveKey#HKDF
  // https://developer.mozilla.org/en-US/docs/Web/API/HkdfParams
  const encryptionKey = await crypto.subtle.deriveKey(
    algorithmOptions,
    material,
    {
      name: ENCRYPTION_ALGORITHM,
      length: 128,
    },
    false,
    ['encrypt', 'decrypt'],
  );

  return { material, encryptionKey };
}

export function createE2EEKey(): Uint8Array {
  return window.crypto.getRandomValues(new Uint8Array(32));
}

/**
 * Ratchets a key. See
 * https://tools.ietf.org/html/draft-omara-sframe-00#section-4.3.5.1
 */
export async function ratchet(material: CryptoKey, salt: string): Promise<ArrayBuffer> {
  const algorithmOptions = getAlgoOptions(material.algorithm.name, salt);

  // https://developer.mozilla.org/en-US/docs/Web/API/SubtleCrypto/deriveBits
  return crypto.subtle.deriveBits(algorithmOptions, material, 256);
}

export function needsRbspUnescaping(frameData: Uint8Array) {
  for (var i = 0; i < frameData.length - 3; i++) {
    if (frameData[i] == 0 && frameData[i + 1] == 0 && frameData[i + 2] == 3) return true;
  }
  return false;
}

export function parseRbsp(stream: Uint8Array): Uint8Array {
  const dataOut: number[] = [];
  var length = stream.length;
  for (var i = 0; i < stream.length; ) {
    // Be careful about over/underflow here. byte_length_ - 3 can underflow, and
    // i + 3 can overflow, but byte_length_ - i can't, because i < byte_length_
    // above, and that expression will produce the number of bytes left in
    // the stream including the byte at i.
    if (length - i >= 3 && !stream[i] && !stream[i + 1] && stream[i + 2] == 3) {
      // Two rbsp bytes.
      dataOut.push(stream[i++]);
      dataOut.push(stream[i++]);
      // Skip the emulation byte.
      i++;
    } else {
      // Single rbsp byte.
      dataOut.push(stream[i++]);
    }
  }
  return new Uint8Array(dataOut);
}

const kZerosInStartSequence = 2;
const kEmulationByte = 3;

export function writeRbsp(data_in: Uint8Array): Uint8Array {
  const dataOut: number[] = [];
  var numConsecutiveZeros = 0;
  for (var i = 0; i < data_in.length; ++i) {
    var byte = data_in[i];
    if (byte <= kEmulationByte && numConsecutiveZeros >= kZerosInStartSequence) {
      // Need to escape.
      dataOut.push(kEmulationByte);
      numConsecutiveZeros = 0;
    }
    dataOut.push(byte);
    if (byte == 0) {
      ++numConsecutiveZeros;
    } else {
      numConsecutiveZeros = 0;
    }
  }
  return new Uint8Array(dataOut);
}

export function asEncryptablePacket(packet: DataPacket): EncryptedPacketPayload | undefined {
  if (
    packet.value?.case !== 'sipDtmf' &&
    packet.value?.case !== 'metrics' &&
    packet.value?.case !== 'speaker' &&
    packet.value?.case !== 'transcription' &&
    packet.value?.case !== 'encryptedPacket'
  ) {
    return new EncryptedPacketPayload({
      value: packet.value,
    });
  }
  return undefined;
}
