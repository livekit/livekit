// SPDX-FileCopyrightText: 2024 LiveKit, Inc.
//
// SPDX-License-Identifier: Apache-2.0
import type { JsonValue } from '@bufbuild/protobuf';

// twirp RPC adapter for client implementation

const defaultPrefix = '/twirp';

export const livekitPackage = 'livekit';
export interface Rpc {
  request(
    service: string,
    method: string,
    data: JsonValue,
    headers: any, // eslint-disable-line @typescript-eslint/no-explicit-any
    timeout?: number,
  ): Promise<string>;
}

export class TwirpError extends Error {
  status: number;
  code?: string;
  metadata?: Record<string, string>;

  constructor(
    name: string,
    message: string,
    status: number,
    code?: string,
    metadata?: Record<string, string>,
  ) {
    super(message);
    this.name = name;
    this.status = status;
    this.code = code;
    this.metadata = metadata;
  }
}

/**
 * JSON based Twirp V7 RPC
 */
export class TwirpRpc {
  host: string;

  pkg: string;

  prefix: string;

  constructor(host: string, pkg: string, prefix?: string) {
    if (host.startsWith('ws')) {
      host = host.replace('ws', 'http');
    }
    this.host = host;
    this.pkg = pkg;
    this.prefix = prefix || defaultPrefix;
  }

  async request(
    service: string,
    method: string,
    data: any, // eslint-disable-line @typescript-eslint/no-explicit-any
    headers: any, // eslint-disable-line @typescript-eslint/no-explicit-any
    timeout = 60,
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
  ): Promise<any> {
    const path = `${this.prefix}/${this.pkg}.${service}/${method}`;
    const url = new URL(path, this.host);
    const init: RequestInit = {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json;charset=UTF-8',
        ...headers,
      },
      body: JSON.stringify(data),
    };

    if (timeout) {
      init.signal = AbortSignal.timeout(timeout * 1000);
    }

    const response = await fetch(url, init);

    if (!response.ok) {
      const isJson = response.headers.get('content-type') === 'application/json';
      let errorMessage = 'Unknown internal error';
      let errorCode: string | undefined = undefined;
      let metadata: Record<string, string> | undefined = undefined;
      try {
        if (isJson) {
          const parsedError = (await response.json()) as Record<string, unknown>;
          if ('msg' in parsedError) {
            errorMessage = <string>parsedError.msg;
          }
          if ('code' in parsedError) {
            errorCode = <string>parsedError.code;
          }
          if ('meta' in parsedError) {
            metadata = <Record<string, string>>parsedError.meta;
          }
        } else {
          errorMessage = await response.text();
        }
      } catch (e) {
        // parsing went wrong, no op and we keep default error message
        console.debug(`Error when trying to parse error message, using defaults`, e);
      }

      throw new TwirpError(response.statusText, errorMessage, response.status, errorCode, metadata);
    }
    const parsedResp = (await response.json()) as Record<string, unknown>;

    const camelcaseKeys = await import('camelcase-keys').then((mod) => mod.default);
    return camelcaseKeys(parsedResp, { deep: true });
  }
}
