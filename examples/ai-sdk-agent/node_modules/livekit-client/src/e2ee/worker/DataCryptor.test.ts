import { describe, expect, it, vitest } from 'vitest';
import { IV_LENGTH, KEY_PROVIDER_DEFAULTS } from '../constants';
import type { KeyProviderOptions } from '../types';
import { createKeyMaterialFromString } from '../utils';
import { DataCryptor } from './DataCryptor';
import { ParticipantKeyHandler } from './ParticipantKeyHandler';

function prepareParticipantTestKeys(
  participantIdentity: string,
  partialKeyProviderOptions: Partial<KeyProviderOptions>,
): ParticipantKeyHandler {
  const keyProviderOptions = { ...KEY_PROVIDER_DEFAULTS, ...partialKeyProviderOptions };
  return new ParticipantKeyHandler(participantIdentity, keyProviderOptions);
}

describe('DataCryptor', () => {
  const participantIdentity = 'testParticipant';

  describe('encrypt', () => {
    it('throws error when no key set', async () => {
      const keys = prepareParticipantTestKeys(participantIdentity, {});
      const data = new Uint8Array([1, 2, 3, 4, 5, 6, 7, 8]);

      await expect(DataCryptor.encrypt(data, keys)).rejects.toThrow('No key set found');
    });

    it('encrypts data successfully with key', async () => {
      const keys = prepareParticipantTestKeys(participantIdentity, {});
      await keys.setKey(await createKeyMaterialFromString('test-key'), 1);

      const plainData = new Uint8Array([1, 2, 3, 4, 5, 6, 7, 8]);
      const result = await DataCryptor.encrypt(plainData, keys);

      expect(result.payload).toBeInstanceOf(Uint8Array);
      expect(result.iv).toBeInstanceOf(Uint8Array);
      expect(result.iv.length).toBe(IV_LENGTH);
      expect(result.keyIndex).toBe(1);
      expect(result.payload).not.toEqual(plainData);
      expect(result.payload.length).toBeGreaterThan(0);
    });

    it('generates different IV for each encryption', async () => {
      const keys = prepareParticipantTestKeys(participantIdentity, {});
      await keys.setKey(await createKeyMaterialFromString('test-key'), 1);

      const data = new Uint8Array([1, 2, 3, 4, 5, 6, 7, 8]);

      const result1 = await DataCryptor.encrypt(data, keys);
      const result2 = await DataCryptor.encrypt(data, keys);

      expect(result1.iv).not.toEqual(result2.iv);
      expect(result1.payload).not.toEqual(result2.payload);
    });

    it('uses correct key index from key handler', async () => {
      const keys = prepareParticipantTestKeys(participantIdentity, {});
      await keys.setKey(await createKeyMaterialFromString('test-key'), 5);

      const data = new Uint8Array([1, 2, 3, 4, 5, 6, 7, 8]);
      const result = await DataCryptor.encrypt(data, keys);

      expect(result.keyIndex).toBe(5);
    });
  });

  describe('decrypt', () => {
    it('throws error when no key set for index', async () => {
      const keys = prepareParticipantTestKeys(participantIdentity, {});
      const data = new Uint8Array([1, 2, 3, 4, 5, 6, 7, 8]);
      const iv = new Uint8Array(IV_LENGTH);

      await expect(DataCryptor.decrypt(data, iv, keys, 1)).rejects.toThrow('No key set found');
    });

    it('decrypts data successfully with correct key', async () => {
      const keys = prepareParticipantTestKeys(participantIdentity, {});
      await keys.setKey(await createKeyMaterialFromString('test-key'), 1);

      const plainData = new Uint8Array([1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16]);

      // First encrypt the data
      const encrypted = await DataCryptor.encrypt(plainData, keys);

      // Then decrypt it
      const decrypted = await DataCryptor.decrypt(
        encrypted.payload,
        encrypted.iv,
        keys,
        encrypted.keyIndex,
      );

      expect(decrypted.payload).toEqual(plainData);
    });

    it('fails to decrypt with incorrect key', async () => {
      const keys1 = prepareParticipantTestKeys('participant1', {});
      const keys2 = prepareParticipantTestKeys('participant2', {});

      await keys1.setKey(await createKeyMaterialFromString('correct-key'), 1);
      await keys2.setKey(await createKeyMaterialFromString('wrong-key'), 1);

      const plainData = new Uint8Array([1, 2, 3, 4, 5, 6, 7, 8]);

      // Encrypt with first key
      const encrypted = await DataCryptor.encrypt(plainData, keys1);

      // Try to decrypt with second (wrong) key
      await expect(
        DataCryptor.decrypt(encrypted.payload, encrypted.iv, keys2, encrypted.keyIndex),
      ).rejects.toThrow();
    });

    it('handles ratcheting when enabled', async () => {
      const senderKeys = prepareParticipantTestKeys('sender', {
        ratchetWindowSize: 2,
      });
      const receiverKeys = prepareParticipantTestKeys('receiver', {
        ratchetWindowSize: 2,
      });

      // Both start with the same initial key
      const initialMaterial = await createKeyMaterialFromString('test-key');
      await senderKeys.setKey(initialMaterial, 1);
      await receiverKeys.setKey(initialMaterial, 1);

      const plainData = new Uint8Array([1, 2, 3, 4, 5, 6, 7, 8]);

      // Sender ratchets their key forward
      await senderKeys.ratchetKey(1, false);

      // Sender encrypts data with the ratcheted key
      const encrypted = await DataCryptor.encrypt(plainData, senderKeys);

      // Receiver should be able to decrypt by automatically ratcheting their key
      const decrypted = await DataCryptor.decrypt(
        encrypted.payload,
        encrypted.iv,
        receiverKeys,
        encrypted.keyIndex,
      );

      expect(decrypted.payload).toEqual(plainData);
    });

    it('respects ratchet window size limit', async () => {
      // Create a scenario where we have valid encrypted data that requires ratcheting but it's disabled
      const senderKeys = prepareParticipantTestKeys('sender', {
        ratchetWindowSize: 10, // Large window for sender
      });
      const receiverKeys = prepareParticipantTestKeys('receiver', {
        ratchetWindowSize: 1, // No ratcheting allowed for receiver
      });

      // Both start with the same initial key
      const initialMaterial = await createKeyMaterialFromString('test-key');
      await senderKeys.setKey(initialMaterial, 1);
      await receiverKeys.setKey(initialMaterial, 1);

      const plainData = new Uint8Array([1, 2, 3, 4, 5, 6, 7, 8]);

      // Sender ratchets their key forward once
      await senderKeys.ratchetKey(1);
      await senderKeys.ratchetKey(1);

      // Sender encrypts data with the ratcheted key
      const encrypted = await DataCryptor.encrypt(plainData, senderKeys);

      // Receiver should fail to decrypt with invalid key because ratcheting is limited (window size 1)
      await expect(
        DataCryptor.decrypt(encrypted.payload, encrypted.iv, receiverKeys, encrypted.keyIndex),
      ).rejects.toThrow('valid key missing for participant');
    });

    it('throws CryptorError when ratcheting disabled and decryption fails', async () => {
      const keys = prepareParticipantTestKeys(participantIdentity, {
        ratchetWindowSize: 0,
      });

      await keys.setKey(await createKeyMaterialFromString('test-key'), 1);

      const invalidData = new Uint8Array([99, 98, 97, 96, 95, 94, 93, 92]);
      const iv = new Uint8Array(IV_LENGTH);
      crypto.getRandomValues(iv);

      await expect(DataCryptor.decrypt(invalidData, iv, keys, 1)).rejects.toThrow(
        'Decryption failed',
      );
    });
  });

  describe('round-trip encryption/decryption', () => {
    it('encrypts and decrypts data correctly', async () => {
      const keys = prepareParticipantTestKeys(participantIdentity, {});
      await keys.setKey(await createKeyMaterialFromString('round-trip-key'), 2);

      const originalData = new Uint8Array([
        1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25,
        26, 27, 28, 29, 30, 31, 32,
      ]);

      const encrypted = await DataCryptor.encrypt(originalData, keys);
      const decrypted = await DataCryptor.decrypt(
        encrypted.payload,
        encrypted.iv,
        keys,
        encrypted.keyIndex,
      );

      expect(decrypted.payload).toEqual(originalData);
    });

    it('handles empty data', async () => {
      const keys = prepareParticipantTestKeys(participantIdentity, {});
      await keys.setKey(await createKeyMaterialFromString('empty-data-key'), 1);

      const emptyData = new Uint8Array(0);

      const encrypted = await DataCryptor.encrypt(emptyData, keys);
      const decrypted = await DataCryptor.decrypt(
        encrypted.payload,
        encrypted.iv,
        keys,
        encrypted.keyIndex,
      );

      expect(decrypted.payload).toEqual(emptyData);
    });

    it('handles large data', async () => {
      const keys = prepareParticipantTestKeys(participantIdentity, {});
      await keys.setKey(await createKeyMaterialFromString('large-data-key'), 1);

      const largeData = new Uint8Array(1024);
      for (let i = 0; i < largeData.length; i++) {
        largeData[i] = i % 256;
      }

      const encrypted = await DataCryptor.encrypt(largeData, keys);
      const decrypted = await DataCryptor.decrypt(
        encrypted.payload,
        encrypted.iv,
        keys,
        encrypted.keyIndex,
      );

      expect(decrypted.payload).toEqual(largeData);
    });
  });

  describe('IV generation', () => {
    it('generates unique IVs with performance.now() timestamp', async () => {
      const keys = prepareParticipantTestKeys(participantIdentity, {});
      await keys.setKey(await createKeyMaterialFromString('iv-test-key'), 1);

      const data = new Uint8Array([1, 2, 3, 4]);

      vitest.useFakeTimers();
      const time1 = 1000;
      vitest.setSystemTime(time1);
      const result1 = await DataCryptor.encrypt(data, keys);

      vitest.setSystemTime(2000);
      const result2 = await DataCryptor.encrypt(data, keys);

      vitest.useRealTimers();

      // IVs should be different due to different timestamps and sendCount
      expect(result1.iv).not.toEqual(result2.iv);
    });
  });
});
