import { EventEmitter } from 'events';
import type TypedEventEmitter from 'typed-emitter';
import log from '../logger';
import { KEY_PROVIDER_DEFAULTS } from './constants';
import { type KeyProviderCallbacks, KeyProviderEvent } from './events';
import type { KeyInfo, KeyProviderOptions, RatchetResult } from './types';
import { createKeyMaterialFromBuffer, createKeyMaterialFromString } from './utils';

/**
 * @experimental
 */
export class BaseKeyProvider extends (EventEmitter as new () => TypedEventEmitter<KeyProviderCallbacks>) {
  private keyInfoMap: Map<string, KeyInfo>;

  private readonly options: KeyProviderOptions;

  constructor(options: Partial<KeyProviderOptions> = {}) {
    super();
    this.keyInfoMap = new Map();
    this.options = { ...KEY_PROVIDER_DEFAULTS, ...options };
    this.on(KeyProviderEvent.KeyRatcheted, this.onKeyRatcheted);
  }

  /**
   * callback to invoke once a key has been set for a participant
   * @param key
   * @param participantIdentity
   * @param keyIndex
   */
  protected onSetEncryptionKey(key: CryptoKey, participantIdentity?: string, keyIndex?: number) {
    const keyInfo: KeyInfo = { key, participantIdentity, keyIndex };
    if (!this.options.sharedKey && !participantIdentity) {
      throw new Error(
        'participant identity needs to be passed for encryption key if sharedKey option is false',
      );
    }
    this.keyInfoMap.set(`${participantIdentity ?? 'shared'}-${keyIndex ?? 0}`, keyInfo);
    this.emit(KeyProviderEvent.SetKey, keyInfo);
  }

  /**
   * Callback being invoked after a key has been ratcheted.
   * Can happen when:
   * - A decryption failure occurs and the key is auto-ratcheted
   * - A ratchet request is sent (see {@link ratchetKey()})
   * @param ratchetResult Contains the ratcheted chain key (exportable to other participants) and the derived new key material.
   * @param participantId
   * @param keyIndex
   */
  protected onKeyRatcheted = (
    ratchetResult: RatchetResult,
    participantId?: string,
    keyIndex?: number,
  ) => {
    log.debug('key ratcheted event received', { ratchetResult, participantId, keyIndex });
  };

  getKeys() {
    return Array.from(this.keyInfoMap.values());
  }

  getOptions() {
    return this.options;
  }

  ratchetKey(participantIdentity?: string, keyIndex?: number) {
    this.emit(KeyProviderEvent.RatchetRequest, participantIdentity, keyIndex);
  }
}

/**
 * A basic KeyProvider implementation intended for a single shared
 * passphrase between all participants
 * @experimental
 */
export class ExternalE2EEKeyProvider extends BaseKeyProvider {
  ratchetInterval: number | undefined;

  constructor(options: Partial<Omit<KeyProviderOptions, 'sharedKey'>> = {}) {
    const opts: Partial<KeyProviderOptions> = {
      ...options,
      sharedKey: true,
      // for a shared key provider failing to decrypt for a specific participant
      // should not mark the key as invalid, so we accept wrong keys forever
      // and won't try to auto-ratchet
      ratchetWindowSize: 0,
      failureTolerance: -1,
    };
    super(opts);
  }

  /**
   * Accepts a passphrase that's used to create the crypto keys.
   * When passing in a string, PBKDF2 is used.
   * When passing in an Array buffer of cryptographically random numbers, HKDF is being used. (recommended)
   * @param key
   */
  async setKey(key: string | ArrayBuffer) {
    const derivedKey =
      typeof key === 'string'
        ? await createKeyMaterialFromString(key)
        : await createKeyMaterialFromBuffer(key);
    this.onSetEncryptionKey(derivedKey);
  }
}
