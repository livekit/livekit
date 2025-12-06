import type Participant from '../room/participant/Participant';
import type { CryptorError } from './errors';
import type { KeyInfo, RatchetResult } from './types';

export enum KeyProviderEvent {
  SetKey = 'setKey',
  /** Event for requesting to ratchet the key used to encrypt the stream */
  RatchetRequest = 'ratchetRequest',
  /** Emitted when a key is ratcheted. Could be after auto-ratcheting on decryption failure or
   *  following a `RatchetRequest`, will contain the ratcheted key material */
  KeyRatcheted = 'keyRatcheted',
}

export type KeyProviderCallbacks = {
  [KeyProviderEvent.SetKey]: (keyInfo: KeyInfo) => void;
  [KeyProviderEvent.RatchetRequest]: (participantIdentity?: string, keyIndex?: number) => void;
  [KeyProviderEvent.KeyRatcheted]: (
    ratchetedResult: RatchetResult,
    participantIdentity?: string,
    keyIndex?: number,
  ) => void;
};

export enum KeyHandlerEvent {
  /** Emitted when a key has been ratcheted. Is emitted when any key has been ratcheted
   * i.e. when the FrameCryptor tried to ratchet when decryption is failing  */
  KeyRatcheted = 'keyRatcheted',
}

export type ParticipantKeyHandlerCallbacks = {
  [KeyHandlerEvent.KeyRatcheted]: (
    ratchetResult: RatchetResult,
    participantIdentity: string,
    keyIndex?: number,
  ) => void;
};

export enum EncryptionEvent {
  ParticipantEncryptionStatusChanged = 'participantEncryptionStatusChanged',
  EncryptionError = 'encryptionError',
}

export type E2EEManagerCallbacks = {
  [EncryptionEvent.ParticipantEncryptionStatusChanged]: (
    enabled: boolean,
    participant: Participant,
  ) => void;
  [EncryptionEvent.EncryptionError]: (error: Error) => void;
};

export type CryptorCallbacks = {
  [CryptorEvent.Error]: (error: CryptorError) => void;
};

export enum CryptorEvent {
  Error = 'cryptorError',
}
