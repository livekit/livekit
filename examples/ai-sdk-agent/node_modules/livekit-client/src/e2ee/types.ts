import type { LogLevel } from '../logger';
import type { VideoCodec } from '../room/track/options';
import type { BaseE2EEManager } from './E2eeManager';
import type { BaseKeyProvider } from './KeyProvider';

export interface BaseMessage {
  kind: string;
  data?: unknown;
}

export interface InitMessage extends BaseMessage {
  kind: 'init';
  data: {
    keyProviderOptions: KeyProviderOptions;
    loglevel: LogLevel;
  };
}

export interface SetKeyMessage extends BaseMessage {
  kind: 'setKey';
  data: {
    participantIdentity?: string;
    isPublisher: boolean;
    key: CryptoKey;
    keyIndex?: number;
  };
}

export interface RTPVideoMapMessage extends BaseMessage {
  kind: 'setRTPMap';
  data: {
    map: Map<number, VideoCodec>;
    participantIdentity: string;
  };
}

export interface SifTrailerMessage extends BaseMessage {
  kind: 'setSifTrailer';
  data: {
    trailer: Uint8Array;
  };
}

export interface EncodeMessage extends BaseMessage {
  kind: 'decode' | 'encode';
  data: {
    participantIdentity: string;
    readableStream: ReadableStream;
    writableStream: WritableStream;
    trackId: string;
    codec?: VideoCodec;
    isReuse: boolean;
  };
}

export interface RemoveTransformMessage extends BaseMessage {
  kind: 'removeTransform';
  data: {
    participantIdentity: string;
    trackId: string;
  };
}

export interface UpdateCodecMessage extends BaseMessage {
  kind: 'updateCodec';
  data: {
    participantIdentity: string;
    trackId: string;
    codec: VideoCodec;
  };
}

export interface RatchetRequestMessage extends BaseMessage {
  kind: 'ratchetRequest';
  data: {
    participantIdentity?: string;
    keyIndex?: number;
  };
}

export interface RatchetMessage extends BaseMessage {
  kind: 'ratchetKey';
  data: {
    participantIdentity: string;
    keyIndex?: number;
    ratchetResult: RatchetResult;
  };
}

export interface ErrorMessage extends BaseMessage {
  kind: 'error';
  data: {
    error: Error;
  };
}

export interface EnableMessage extends BaseMessage {
  kind: 'enable';
  data: {
    participantIdentity: string;
    enabled: boolean;
  };
}

export interface InitAck extends BaseMessage {
  kind: 'initAck';
  data: {
    enabled: boolean;
  };
}

export interface DecryptDataRequestMessage extends BaseMessage {
  kind: 'decryptDataRequest';
  data: {
    uuid: string;
    payload: Uint8Array;
    iv: Uint8Array;
    participantIdentity: string;
    keyIndex: number;
  };
}

export interface DecryptDataResponseMessage extends BaseMessage {
  kind: 'decryptDataResponse';
  data: {
    uuid: string;
    payload: Uint8Array;
  };
}

export interface EncryptDataRequestMessage extends BaseMessage {
  kind: 'encryptDataRequest';
  data: {
    uuid: string;
    payload: Uint8Array;
    participantIdentity: string;
  };
}

export interface EncryptDataResponseMessage extends BaseMessage {
  kind: 'encryptDataResponse';
  data: {
    uuid: string;
    payload: Uint8Array;
    iv: Uint8Array;
    keyIndex: number;
  };
}

export type E2EEWorkerMessage =
  | InitMessage
  | SetKeyMessage
  | EncodeMessage
  | ErrorMessage
  | EnableMessage
  | RemoveTransformMessage
  | RTPVideoMapMessage
  | UpdateCodecMessage
  | RatchetRequestMessage
  | RatchetMessage
  | SifTrailerMessage
  | InitAck
  | DecryptDataRequestMessage
  | DecryptDataResponseMessage
  | EncryptDataRequestMessage
  | EncryptDataResponseMessage;

export type KeySet = { material: CryptoKey; encryptionKey: CryptoKey };

export type RatchetResult = {
  // The ratchet chain key, which is used to derive the next key.
  // Can be shared/exported to other participants.
  chainKey: ArrayBuffer;
  cryptoKey: CryptoKey;
};

export type KeyProviderOptions = {
  sharedKey: boolean;
  ratchetSalt: string;
  ratchetWindowSize: number;
  failureTolerance: number;
  keyringSize: number;
};

export type KeyInfo = {
  key: CryptoKey;
  participantIdentity?: string;
  keyIndex?: number;
};

export type E2EEManagerOptions = {
  keyProvider: BaseKeyProvider;
  worker: Worker;
};

export type E2EEOptions =
  | E2EEManagerOptions
  | {
      /** For react-native usage. */
      e2eeManager: BaseE2EEManager;
    };

export type DecodeRatchetOptions = {
  /** attempts  */
  ratchetCount: number;
  /** ratcheted key to try */
  encryptionKey?: CryptoKey;
};

export type ScriptTransformOptions = {
  kind: 'decode' | 'encode';
  participantIdentity: string;
  trackId: string;
  codec?: VideoCodec;
};
