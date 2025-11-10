import type { KeyProviderOptions } from './types';

export const ENCRYPTION_ALGORITHM = 'AES-GCM';

// How many consecutive frames can fail decrypting before a particular key gets marked as invalid
export const DECRYPTION_FAILURE_TOLERANCE = 10;

// We copy the first bytes of the VP8 payload unencrypted.
// For keyframes this is 10 bytes, for non-keyframes (delta) 3. See
//   https://tools.ietf.org/html/rfc6386#section-9.1
// This allows the bridge to continue detecting keyframes (only one byte needed in the JVB)
// and is also a bit easier for the VP8 decoder (i.e. it generates funny garbage pictures
// instead of being unable to decode).
// This is a bit for show and we might want to reduce to 1 unconditionally in the final version.
//
// For audio (where frame.type is not set) we do not encrypt the opus TOC byte:
//   https://tools.ietf.org/html/rfc6716#section-3.1
export const UNENCRYPTED_BYTES = {
  key: 10,
  delta: 3,
  audio: 1, // frame.type is not set on audio, so this is set manually
  empty: 0,
} as const;

/* We use a 12 byte bit IV. This is signalled in plain together with the
 packet. See https://developer.mozilla.org/en-US/docs/Web/API/SubtleCrypto/encrypt#parameters */
export const IV_LENGTH = 12;

// flag set to indicate that e2ee has been setup for sender/receiver;
export const E2EE_FLAG = 'lk_e2ee';

export const SALT = 'LKFrameEncryptionKey';

export const KEY_PROVIDER_DEFAULTS: KeyProviderOptions = {
  sharedKey: false,
  ratchetSalt: SALT,
  ratchetWindowSize: 8,
  failureTolerance: DECRYPTION_FAILURE_TOLERANCE,
  keyringSize: 16,
} as const;

export const MAX_SIF_COUNT = 100;
export const MAX_SIF_DURATION = 2000;
