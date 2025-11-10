import { EventEmitter } from 'events';
import type TypedEventEmitter from 'typed-emitter';
import { workerLogger } from '../../logger';
import { KeyHandlerEvent, type ParticipantKeyHandlerCallbacks } from '../events';
import type { KeyProviderOptions, KeySet, RatchetResult } from '../types';
import { deriveKeys, importKey, ratchet } from '../utils';

// TODO ParticipantKeyHandlers currently don't get destroyed on participant disconnect
// we could do this by having a separate worker message on participant disconnected.

/**
 * ParticipantKeyHandler is responsible for providing a cryptor instance with the
 * en-/decryption key of a participant. It assumes that all tracks of a specific participant
 * are encrypted with the same key.
 * Additionally it exposes a method to ratchet a key which can be used by the cryptor either automatically
 * if decryption fails or can be triggered manually on both sender and receiver side.
 *
 */
export class ParticipantKeyHandler extends (EventEmitter as new () => TypedEventEmitter<ParticipantKeyHandlerCallbacks>) {
  private currentKeyIndex: number;

  private cryptoKeyRing: Array<KeySet | undefined>;

  private decryptionFailureCounts: Array<number>;

  private ratchetPromiseMap: Map<number, Promise<RatchetResult>>;

  readonly participantIdentity: string;

  /** @internal */
  readonly keyProviderOptions: KeyProviderOptions;

  /**
   * true if the current key has not been marked as invalid
   */
  get hasValidKey(): boolean {
    return !this.hasInvalidKeyAtIndex(this.currentKeyIndex);
  }

  constructor(participantIdentity: string, keyProviderOptions: KeyProviderOptions) {
    super();
    this.currentKeyIndex = 0;
    if (keyProviderOptions.keyringSize < 1 || keyProviderOptions.keyringSize > 256) {
      throw new TypeError('Keyring size needs to be between 1 and 256');
    }
    this.cryptoKeyRing = new Array(keyProviderOptions.keyringSize).fill(undefined);
    this.decryptionFailureCounts = new Array(keyProviderOptions.keyringSize).fill(0);
    this.keyProviderOptions = keyProviderOptions;
    this.ratchetPromiseMap = new Map();
    this.participantIdentity = participantIdentity;
  }

  /**
   * Returns true if the key at the given index is marked as invalid.
   *
   * @param keyIndex the index of the key
   */
  hasInvalidKeyAtIndex(keyIndex: number): boolean {
    return (
      this.keyProviderOptions.failureTolerance >= 0 &&
      this.decryptionFailureCounts[keyIndex] > this.keyProviderOptions.failureTolerance
    );
  }

  /**
   * Informs the key handler that a decryption failure occurred for an encryption key.
   * @internal
   * @param keyIndex the key index for which the failure occurred. Defaults to the current key index.
   */
  decryptionFailure(keyIndex: number = this.currentKeyIndex): void {
    if (this.keyProviderOptions.failureTolerance < 0) {
      return;
    }

    this.decryptionFailureCounts[keyIndex] += 1;

    if (this.decryptionFailureCounts[keyIndex] > this.keyProviderOptions.failureTolerance) {
      workerLogger.warn(
        `key for ${this.participantIdentity} at index ${keyIndex} is being marked as invalid`,
      );
    }
  }

  /**
   * Informs the key handler that a frame was successfully decrypted using an encryption key.
   * @internal
   * @param keyIndex the key index for which the success occurred. Defaults to the current key index.
   */
  decryptionSuccess(keyIndex: number = this.currentKeyIndex): void {
    this.resetKeyStatus(keyIndex);
  }

  /**
   * Call this after user initiated ratchet or a new key has been set in order to make sure to mark potentially
   * invalid keys as valid again
   *
   * @param keyIndex the index of the key. Defaults to the current key index.
   */
  resetKeyStatus(keyIndex?: number): void {
    if (keyIndex === undefined) {
      this.decryptionFailureCounts.fill(0);
    } else {
      this.decryptionFailureCounts[keyIndex] = 0;
    }
  }

  /**
   * Ratchets the current key (or the one at keyIndex if provided) and
   * returns the ratcheted material
   * if `setKey` is true (default), it will also set the ratcheted key directly on the crypto key ring
   * @param keyIndex
   * @param setKey
   */
  ratchetKey(keyIndex?: number, setKey = true): Promise<RatchetResult> {
    const currentKeyIndex = keyIndex ?? this.getCurrentKeyIndex();

    const existingPromise = this.ratchetPromiseMap.get(currentKeyIndex);
    if (typeof existingPromise !== 'undefined') {
      return existingPromise;
    }
    const ratchetPromise = new Promise<RatchetResult>(async (resolve, reject) => {
      try {
        const keySet = this.getKeySet(currentKeyIndex);
        if (!keySet) {
          throw new TypeError(
            `Cannot ratchet key without a valid keyset of participant ${this.participantIdentity}`,
          );
        }
        const currentMaterial = keySet.material;
        const chainKey = await ratchet(currentMaterial, this.keyProviderOptions.ratchetSalt);
        const newMaterial = await importKey(chainKey, currentMaterial.algorithm.name, 'derive');
        const ratchetResult: RatchetResult = {
          chainKey,
          cryptoKey: newMaterial,
        };
        if (setKey) {
          // Set the new key and emit a ratchet event with the ratcheted chain key
          await this.setKeyFromMaterial(newMaterial, currentKeyIndex, ratchetResult);
        }
        resolve(ratchetResult);
      } catch (e) {
        reject(e);
      } finally {
        this.ratchetPromiseMap.delete(currentKeyIndex);
      }
    });
    this.ratchetPromiseMap.set(currentKeyIndex, ratchetPromise);
    return ratchetPromise;
  }

  /**
   * takes in a key material with `deriveBits` and `deriveKey` set as key usages
   * and derives encryption keys from the material and sets it on the key ring buffer
   * together with the material
   * also resets the valid key property and updates the currentKeyIndex
   */
  async setKey(material: CryptoKey, keyIndex = 0) {
    await this.setKeyFromMaterial(material, keyIndex);
    this.resetKeyStatus(keyIndex);
  }

  /**
   * takes in a key material with `deriveBits` and `deriveKey` set as key usages
   * and derives encryption keys from the material and sets it on the key ring buffers
   * together with the material
   * also updates the currentKeyIndex
   */
  async setKeyFromMaterial(
    material: CryptoKey,
    keyIndex: number,
    ratchetedResult: RatchetResult | null = null,
  ) {
    const keySet = await deriveKeys(material, this.keyProviderOptions.ratchetSalt);
    const newIndex = keyIndex >= 0 ? keyIndex % this.cryptoKeyRing.length : this.currentKeyIndex;
    workerLogger.debug(`setting new key with index ${keyIndex}`, {
      usage: material.usages,
      algorithm: material.algorithm,
      ratchetSalt: this.keyProviderOptions.ratchetSalt,
    });
    this.setKeySet(keySet, newIndex, ratchetedResult);
    if (newIndex >= 0) this.currentKeyIndex = newIndex;
  }

  setKeySet(keySet: KeySet, keyIndex: number, ratchetedResult: RatchetResult | null = null) {
    this.cryptoKeyRing[keyIndex % this.cryptoKeyRing.length] = keySet;

    if (ratchetedResult) {
      this.emit(KeyHandlerEvent.KeyRatcheted, ratchetedResult, this.participantIdentity, keyIndex);
    }
  }

  async setCurrentKeyIndex(index: number) {
    this.currentKeyIndex = index % this.cryptoKeyRing.length;
    this.resetKeyStatus(index);
  }

  getCurrentKeyIndex() {
    return this.currentKeyIndex;
  }

  /**
   * returns currently used KeySet or the one at `keyIndex` if provided
   * @param keyIndex
   * @returns
   */
  getKeySet(keyIndex?: number) {
    return this.cryptoKeyRing[keyIndex ?? this.currentKeyIndex];
  }
}
