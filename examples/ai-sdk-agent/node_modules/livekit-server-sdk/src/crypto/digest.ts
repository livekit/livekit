// SPDX-FileCopyrightText: 2024 LiveKit, Inc.
//
// SPDX-License-Identifier: Apache-2.0

// Use the Web Crypto API if available, otherwise fallback to Node.js crypto
export async function digest(data: string): Promise<ArrayBuffer> {
  if (globalThis.crypto?.subtle) {
    const encoder = new TextEncoder();
    return crypto.subtle.digest('SHA-256', encoder.encode(data));
  } else {
    const nodeCrypto = await import('node:crypto');
    return nodeCrypto.createHash('sha256').update(data).digest();
  }
}
