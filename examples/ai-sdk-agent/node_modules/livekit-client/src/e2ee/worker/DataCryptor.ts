import { workerLogger } from '../../logger';
import { ENCRYPTION_ALGORITHM } from '../constants';
import { CryptorError, CryptorErrorReason } from '../errors';
import type { DecodeRatchetOptions, KeySet, RatchetResult } from '../types';
import { deriveKeys } from '../utils';
import type { ParticipantKeyHandler } from './ParticipantKeyHandler';

export class DataCryptor {
  private static sendCount = 0;

  private static makeIV(timestamp: number) {
    const iv = new ArrayBuffer(12);
    const ivView = new DataView(iv);
    const randomBytes = crypto.getRandomValues(new Uint32Array(1));
    ivView.setUint32(0, randomBytes[0]);
    ivView.setUint32(4, timestamp);
    ivView.setUint32(8, timestamp - (DataCryptor.sendCount % 0xffff));
    DataCryptor.sendCount++;

    return iv;
  }

  static async encrypt(
    data: Uint8Array,
    keys: ParticipantKeyHandler,
  ): Promise<{
    payload: Uint8Array;
    iv: Uint8Array;
    keyIndex: number;
  }> {
    const iv = DataCryptor.makeIV(performance.now());
    const keySet = await keys.getKeySet();
    if (!keySet) {
      throw new Error('No key set found');
    }

    const cipherText = await crypto.subtle.encrypt(
      {
        name: ENCRYPTION_ALGORITHM,
        iv,
      },
      keySet.encryptionKey,
      new Uint8Array(data),
    );

    return {
      payload: new Uint8Array(cipherText),
      iv: new Uint8Array(iv),
      keyIndex: keys.getCurrentKeyIndex(),
    };
  }

  static async decrypt(
    data: Uint8Array,
    iv: Uint8Array,
    keys: ParticipantKeyHandler,
    keyIndex: number = 0,
    initialMaterial?: KeySet,
    ratchetOpts: DecodeRatchetOptions = { ratchetCount: 0 },
  ): Promise<{
    payload: Uint8Array;
  }> {
    const keySet = await keys.getKeySet(keyIndex);
    if (!keySet) {
      throw new Error('No key set found');
    }

    try {
      const plainText = await crypto.subtle.decrypt(
        {
          name: ENCRYPTION_ALGORITHM,
          iv,
        },
        keySet.encryptionKey,
        new Uint8Array(data),
      );
      return {
        payload: new Uint8Array(plainText),
      };
    } catch (error: any) {
      if (keys.keyProviderOptions.ratchetWindowSize > 0) {
        if (ratchetOpts.ratchetCount < keys.keyProviderOptions.ratchetWindowSize) {
          workerLogger.debug(
            `DataCryptor: ratcheting key attempt ${ratchetOpts.ratchetCount} of ${
              keys.keyProviderOptions.ratchetWindowSize
            }, for data packet`,
          );

          let ratchetedKeySet: KeySet | undefined;
          let ratchetResult: RatchetResult | undefined;
          if ((initialMaterial ?? keySet) === keys.getKeySet(keyIndex)) {
            // only ratchet if the currently set key is still the same as the one used to decrypt this frame
            // if not, it might be that a different frame has already ratcheted and we try with that one first
            ratchetResult = await keys.ratchetKey(keyIndex, false);

            ratchetedKeySet = await deriveKeys(
              ratchetResult.cryptoKey,
              keys.keyProviderOptions.ratchetSalt,
            );
          }

          const decryptedData = await DataCryptor.decrypt(
            data,
            iv,
            keys,
            keyIndex,
            initialMaterial,
            {
              ratchetCount: ratchetOpts.ratchetCount + 1,
              encryptionKey: ratchetedKeySet?.encryptionKey,
            },
          );

          if (decryptedData && ratchetedKeySet) {
            // before updating the keys, make sure that the keySet used for this frame is still the same as the currently set key
            // if it's not, a new key might have been set already, which we don't want to override
            if ((initialMaterial ?? keySet) === keys.getKeySet(keyIndex)) {
              keys.setKeySet(ratchetedKeySet, keyIndex, ratchetResult);
              // decryption was successful, set the new key index to reflect the ratcheted key set
              keys.setCurrentKeyIndex(keyIndex);
            }
          }
          return decryptedData;
        } else {
          /**
           * Because we only set a new key once decryption has been successful,
           * we can be sure that we don't need to reset the key to the initial material at this point
           * as the key has not been updated on the keyHandler instance
           */

          workerLogger.warn('DataCryptor: maximum ratchet attempts exceeded');
          throw new CryptorError(
            `DataCryptor: valid key missing for participant ${keys.participantIdentity}`,
            CryptorErrorReason.InvalidKey,
            keys.participantIdentity,
          );
        }
      } else {
        throw new CryptorError(
          `DataCryptor: Decryption failed: ${error.message}`,
          CryptorErrorReason.InvalidKey,
          keys.participantIdentity,
        );
      }
    }
  }
}
