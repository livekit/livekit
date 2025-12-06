// SPDX-FileCopyrightText: 2024 LiveKit, Inc.
//
// SPDX-License-Identifier: Apache-2.0

// Use the Web Crypto API if available, otherwise fallback to Node.js crypto
export async function getRandomBytes(size: number = 16): Promise<Uint8Array> {
  if (globalThis.crypto) {
    return crypto.getRandomValues(new Uint8Array(size));
  } else {
    const nodeCrypto = await import('node:crypto');
    return nodeCrypto.getRandomValues(new Uint8Array(size));
  }
}
