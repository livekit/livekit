import type { VideoCodec } from '../..';

//  Payload definitions taken from https://github.com/livekit/livekit/blob/master/pkg/sfu/downtrack.go#L104

export const VP8KeyFrame8x8: Uint8Array = new Uint8Array([
  0x10, 0x02, 0x00, 0x9d, 0x01, 0x2a, 0x08, 0x00, 0x08, 0x00, 0x00, 0x47, 0x08, 0x85, 0x85, 0x88,
  0x85, 0x84, 0x88, 0x02, 0x02, 0x00, 0x0c, 0x0d, 0x60, 0x00, 0xfe, 0xff, 0xab, 0x50, 0x80,
]);

export const H264KeyFrame2x2SPS: Uint8Array = new Uint8Array([
  0x67, 0x42, 0xc0, 0x1f, 0x0f, 0xd9, 0x1f, 0x88, 0x88, 0x84, 0x00, 0x00, 0x03, 0x00, 0x04, 0x00,
  0x00, 0x03, 0x00, 0xc8, 0x3c, 0x60, 0xc9, 0x20,
]);

export const H264KeyFrame2x2PPS: Uint8Array = new Uint8Array([0x68, 0x87, 0xcb, 0x83, 0xcb, 0x20]);

export const H264KeyFrame2x2IDR: Uint8Array = new Uint8Array([
  0x65, 0x88, 0x84, 0x0a, 0xf2, 0x62, 0x80, 0x00, 0xa7, 0xbe,
]);

export const H264KeyFrame2x2: Uint8Array[] = [
  H264KeyFrame2x2SPS,
  H264KeyFrame2x2PPS,
  H264KeyFrame2x2IDR,
];

export const OpusSilenceFrame: Uint8Array = new Uint8Array([
  0xf8, 0xff, 0xfe, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
  0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
  0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
  0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
  0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
]);

/**
 * Create a crypto hash using Web Crypto API for secure comparison operations
 */
async function cryptoHash(data: Uint8Array | ArrayBuffer): Promise<string> {
  const hashBuffer = await crypto.subtle.digest('SHA-256', data);
  const hashArray = new Uint8Array(hashBuffer);
  return Array.from(hashArray)
    .map((b) => b.toString(16).padStart(2, '0'))
    .join('');
}

/**
 * Pre-computed SHA-256 hashes for secure comparison operations
 */
export const CryptoHashes = {
  VP8KeyFrame8x8: 'ef0161653d8b2b23aad46624b420af1d03ce48950e9fc85718028f91b50f9219',
  H264KeyFrame2x2SPS: 'f0a0e09647d891d6d50aa898bce7108090375d0d55e50a2bb21147afee558e44',
  H264KeyFrame2x2PPS: '61d9665eed71b6d424ae9539330a3bdd5cb386d4d781c808219a6e36750493a7',
  H264KeyFrame2x2IDR: 'faffc26b68a2fc09096fa20f3351e706398b6f838a7500c8063472c2e476e90d',
  OpusSilenceFrame: 'aad8d31fc56b2802ca500e58c2fb9d0b29ad71bb7cb52cd6530251eade188988',
} as const;

/**
 * Check if a byte array matches any of the known SIF payload frame types using secure crypto hashes
 */
export async function identifySifPayload(
  data: Uint8Array | ArrayBuffer,
): Promise<VideoCodec | 'opus' | null> {
  const hash = await cryptoHash(data);

  switch (hash) {
    case CryptoHashes.VP8KeyFrame8x8:
      return 'vp8';
    case CryptoHashes.H264KeyFrame2x2SPS:
      return 'h264';
    case CryptoHashes.H264KeyFrame2x2PPS:
      return 'h264';
    case CryptoHashes.H264KeyFrame2x2IDR:
      return 'h264';
    case CryptoHashes.OpusSilenceFrame:
      return 'opus';
    default:
      return null;
  }
}
