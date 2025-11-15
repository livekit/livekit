import { Mutex } from '@livekit/mutex';
import {
  RoomAgentDispatch,
  RoomConfiguration,
  TokenSourceRequest,
  TokenSourceResponse,
} from '@livekit/protocol';
import {
  TokenSourceConfigurable,
  type TokenSourceFetchOptions,
  TokenSourceFixed,
  type TokenSourceResponseObject,
} from './types';
import { decodeTokenPayload, isResponseTokenValid } from './utils';

/** A TokenSourceCached is a TokenSource which caches the last {@link TokenSourceResponseObject} value and returns it
 * until a) it expires or b) the {@link TokenSourceFetchOptions} provided to .fetch(...) change. */
abstract class TokenSourceCached extends TokenSourceConfigurable {
  private cachedFetchOptions: TokenSourceFetchOptions | null = null;

  private cachedResponse: TokenSourceResponse | null = null;

  private fetchMutex = new Mutex();

  private isSameAsCachedFetchOptions(options: TokenSourceFetchOptions) {
    if (!this.cachedFetchOptions) {
      return false;
    }

    for (const key of Object.keys(this.cachedFetchOptions) as Array<
      keyof TokenSourceFetchOptions
    >) {
      switch (key) {
        case 'roomName':
        case 'participantName':
        case 'participantIdentity':
        case 'participantMetadata':
        case 'participantAttributes':
        case 'agentName':
        case 'agentMetadata':
          if (this.cachedFetchOptions[key] !== options[key]) {
            return false;
          }
          break;
        default:
          // ref: https://stackoverflow.com/a/58009992
          const exhaustiveCheckedKey: never = key;
          throw new Error(`Options key ${exhaustiveCheckedKey} not being checked for equality!`);
      }
    }

    return true;
  }

  private shouldReturnCachedValueFromFetch(fetchOptions: TokenSourceFetchOptions) {
    if (!this.cachedResponse) {
      return false;
    }
    if (!isResponseTokenValid(this.cachedResponse)) {
      return false;
    }
    if (this.isSameAsCachedFetchOptions(fetchOptions)) {
      return false;
    }
    return true;
  }

  getCachedResponseJwtPayload() {
    if (!this.cachedResponse) {
      return null;
    }
    return decodeTokenPayload(this.cachedResponse.participantToken);
  }

  async fetch(options: TokenSourceFetchOptions): Promise<TokenSourceResponseObject> {
    const unlock = await this.fetchMutex.lock();
    try {
      if (this.shouldReturnCachedValueFromFetch(options)) {
        return this.cachedResponse!.toJson() as TokenSourceResponseObject;
      }
      this.cachedFetchOptions = options;

      const tokenResponse = await this.update(options);
      this.cachedResponse = tokenResponse;
      return tokenResponse.toJson() as TokenSourceResponseObject;
    } finally {
      unlock();
    }
  }

  protected abstract update(options: TokenSourceFetchOptions): Promise<TokenSourceResponse>;
}

type LiteralOrFn =
  | TokenSourceResponseObject
  | (() => TokenSourceResponseObject | Promise<TokenSourceResponseObject>);
class TokenSourceLiteral extends TokenSourceFixed {
  private literalOrFn: LiteralOrFn;

  constructor(literalOrFn: LiteralOrFn) {
    super();
    this.literalOrFn = literalOrFn;
  }

  async fetch(): Promise<TokenSourceResponseObject> {
    if (typeof this.literalOrFn === 'function') {
      return this.literalOrFn();
    } else {
      return this.literalOrFn;
    }
  }
}

type CustomFn = (
  options: TokenSourceFetchOptions,
) => TokenSourceResponseObject | Promise<TokenSourceResponseObject>;
class TokenSourceCustom extends TokenSourceCached {
  private customFn: CustomFn;

  constructor(customFn: CustomFn) {
    super();
    this.customFn = customFn;
  }

  protected async update(options: TokenSourceFetchOptions) {
    const resultMaybePromise = this.customFn(options);

    let result;
    if (resultMaybePromise instanceof Promise) {
      result = await resultMaybePromise;
    } else {
      result = resultMaybePromise;
    }

    return TokenSourceResponse.fromJson(result, {
      // NOTE: it could be possible that the response body could contain more fields than just
      // what's in TokenSourceResponse depending on the implementation
      ignoreUnknownFields: true,
    });
  }
}

export type EndpointOptions = Omit<RequestInit, 'body'>;

class TokenSourceEndpoint extends TokenSourceCached {
  private url: string;

  private endpointOptions: EndpointOptions;

  constructor(url: string, options: EndpointOptions = {}) {
    super();
    this.url = url;
    this.endpointOptions = options;
  }

  private createRequestFromOptions(options: TokenSourceFetchOptions) {
    const request = new TokenSourceRequest();

    for (const key of Object.keys(options) as Array<keyof TokenSourceFetchOptions>) {
      switch (key) {
        case 'roomName':
        case 'participantName':
        case 'participantIdentity':
        case 'participantMetadata':
          request[key] = options[key];
          break;

        case 'participantAttributes':
          request.participantAttributes = options.participantAttributes ?? {};
          break;

        case 'agentName':
          request.roomConfig = request.roomConfig ?? new RoomConfiguration();
          if (request.roomConfig.agents.length === 0) {
            request.roomConfig.agents.push(new RoomAgentDispatch());
          }
          request.roomConfig.agents[0].agentName = options.agentName!;
          break;

        case 'agentMetadata':
          request.roomConfig = request.roomConfig ?? new RoomConfiguration();
          if (request.roomConfig.agents.length === 0) {
            request.roomConfig.agents.push(new RoomAgentDispatch());
          }
          request.roomConfig.agents[0].metadata = options.agentMetadata!;
          break;

        default:
          // ref: https://stackoverflow.com/a/58009992
          const exhaustiveCheckedKey: never = key;
          throw new Error(
            `Options key ${exhaustiveCheckedKey} not being included in forming request!`,
          );
      }
    }

    return request;
  }

  protected async update(options: TokenSourceFetchOptions) {
    const request = this.createRequestFromOptions(options);

    const response = await fetch(this.url, {
      ...this.endpointOptions,
      method: this.endpointOptions.method ?? 'POST',
      headers: {
        'Content-Type': 'application/json',
        ...this.endpointOptions.headers,
      },
      body: request.toJsonString({
        useProtoFieldName: true,
      }),
    });

    if (!response.ok) {
      throw new Error(
        `Error generating token from endpoint ${this.url}: received ${response.status} / ${await response.text()}`,
      );
    }

    const body = await response.json();
    return TokenSourceResponse.fromJson(body, {
      // NOTE: it could be possible that the response body could contain more fields than just
      // what's in TokenSourceResponse depending on the implementation (ie, SandboxTokenServer)
      ignoreUnknownFields: true,
    });
  }
}

export type SandboxTokenServerOptions = {
  baseUrl?: string;
};

class TokenSourceSandboxTokenServer extends TokenSourceEndpoint {
  constructor(sandboxId: string, options: SandboxTokenServerOptions) {
    const { baseUrl = 'https://cloud-api.livekit.io', ...rest } = options;

    super(`${baseUrl}/api/v2/sandbox/connection-details`, {
      ...rest,
      headers: {
        'X-Sandbox-ID': sandboxId,
      },
    });
  }
}

export {
  type TokenSourceLiteral,
  type TokenSourceCustom,
  type TokenSourceEndpoint,
  type TokenSourceSandboxTokenServer,
  decodeTokenPayload,
};

export const TokenSource = {
  /** TokenSource.literal contains a single, literal set of {@link TokenSourceResponseObject}
   * credentials, either provided directly or returned from a provided function. */
  literal(literalOrFn: LiteralOrFn) {
    return new TokenSourceLiteral(literalOrFn);
  },

  /**
   * TokenSource.custom allows a user to define a manual function which generates new
   * {@link TokenSourceResponseObject} values on demand.
   *
   * Use this to get credentials from custom backends / etc.
   */
  custom(customFn: CustomFn) {
    return new TokenSourceCustom(customFn);
  },

  /**
   * TokenSource.endpoint creates a token source that fetches credentials from a given URL using
   * the standard endpoint format:
   * FIXME: add docs link here in the future!
   */
  endpoint(url: string, options: EndpointOptions = {}) {
    return new TokenSourceEndpoint(url, options);
  },

  /**
   * TokenSource.sandboxTokenServer queries a sandbox token server for credentials,
   * which supports quick prototyping / getting started types of use cases.
   *
   * This token provider is INSECURE and should NOT be used in production.
   *
   * For more info:
   * @see https://cloud.livekit.io/projects/p_/sandbox/templates/token-server
   */
  sandboxTokenServer(sandboxId: string, options: SandboxTokenServerOptions = {}) {
    return new TokenSourceSandboxTokenServer(sandboxId, options);
  },
};
