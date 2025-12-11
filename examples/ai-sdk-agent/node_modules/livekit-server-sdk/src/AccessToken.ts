// SPDX-FileCopyrightText: 2024 LiveKit, Inc.
//
// SPDX-License-Identifier: Apache-2.0
import type { RoomConfiguration } from '@livekit/protocol';
import * as jose from 'jose';
import type { ClaimGrants, InferenceGrant, SIPGrant, VideoGrant } from './grants.js';
import { claimsToJwtPayload } from './grants.js';

// 6 hours
const defaultTTL = `6h`;

const defaultClockToleranceSeconds = 10;

export interface AccessTokenOptions {
  /**
   * amount of time before expiration
   * expressed in seconds or a string describing a time span zeit/ms.
   * eg: '2 days', '10h', or seconds as numeric value
   */
  ttl?: number | string;

  /**
   * display name for the participant, available as `Participant.name`
   */
  name?: string;

  /**
   * identity of the user, required for room join tokens
   */
  identity?: string;

  /**
   * custom participant metadata
   */
  metadata?: string;

  /**
   * custom participant attributes
   */
  attributes?: Record<string, string>;
}

export class AccessToken {
  private apiKey: string;

  private apiSecret: string;

  private grants: ClaimGrants;

  identity?: string;

  ttl: number | string;

  /**
   * Creates a new AccessToken
   * @param apiKey - API Key, can be set in env LIVEKIT_API_KEY
   * @param apiSecret - Secret, can be set in env LIVEKIT_API_SECRET
   */
  constructor(apiKey?: string, apiSecret?: string, options?: AccessTokenOptions) {
    if (!apiKey) {
      apiKey = process.env.LIVEKIT_API_KEY;
    }
    if (!apiSecret) {
      apiSecret = process.env.LIVEKIT_API_SECRET;
    }
    if (!apiKey || !apiSecret) {
      throw Error('api-key and api-secret must be set');
    }
    // @ts-expect-error we're not including dom lib for the server sdk so document is not defined
    else if (typeof document !== 'undefined') {
      // check against document rather than window because deno provides window
      console.error(
        'You should not include your API secret in your web client bundle.\n\n' +
          'Your web client should request a token from your backend server which should then use ' +
          'the API secret to generate a token. See https://docs.livekit.io/client/connect/',
      );
    }
    this.apiKey = apiKey;
    this.apiSecret = apiSecret;
    this.grants = {};
    this.identity = options?.identity;
    this.ttl = options?.ttl || defaultTTL;
    if (typeof this.ttl === 'number') {
      this.ttl = `${this.ttl}s`;
    }
    if (options?.metadata) {
      this.metadata = options.metadata;
    }
    if (options?.attributes) {
      this.attributes = options.attributes;
    }
    if (options?.name) {
      this.name = options.name;
    }
  }

  /**
   * Adds a video grant to this token.
   * @param grant -
   */
  addGrant(grant: VideoGrant) {
    this.grants.video = { ...(this.grants.video ?? {}), ...grant };
  }

  /**
   * Adds an inference grant to this token.
   * @param grant -
   */
  addInferenceGrant(grant: InferenceGrant) {
    this.grants.inference = { ...(this.grants.inference ?? {}), ...grant };
  }

  /**
   * Adds a SIP grant to this token.
   * @param grant -
   */
  addSIPGrant(grant: SIPGrant) {
    this.grants.sip = { ...(this.grants.sip ?? {}), ...grant };
  }

  get name(): string | undefined {
    return this.grants.name;
  }

  set name(name: string) {
    this.grants.name = name;
  }

  get metadata(): string | undefined {
    return this.grants.metadata;
  }

  /**
   * Set metadata to be passed to the Participant, used only when joining the room
   */
  set metadata(md: string) {
    this.grants.metadata = md;
  }

  get attributes(): Record<string, string> | undefined {
    return this.grants.attributes;
  }

  set attributes(attrs: Record<string, string>) {
    this.grants.attributes = attrs;
  }

  get kind(): string | undefined {
    return this.grants.kind;
  }

  set kind(kind: string) {
    this.grants.kind = kind;
  }

  get sha256(): string | undefined {
    return this.grants.sha256;
  }

  set sha256(sha: string | undefined) {
    this.grants.sha256 = sha;
  }

  get roomPreset(): string | undefined {
    return this.grants.roomPreset;
  }

  set roomPreset(preset: string | undefined) {
    this.grants.roomPreset = preset;
  }

  get roomConfig(): RoomConfiguration | undefined {
    return this.grants.roomConfig;
  }

  set roomConfig(config: RoomConfiguration | undefined) {
    this.grants.roomConfig = config;
  }

  /**
   * @returns JWT encoded token
   */
  async toJwt(): Promise<string> {
    // TODO: check for video grant validity

    const secret = new TextEncoder().encode(this.apiSecret);

    const jwt = new jose.SignJWT(claimsToJwtPayload(this.grants))
      .setProtectedHeader({ alg: 'HS256' })
      .setIssuer(this.apiKey)
      .setExpirationTime(this.ttl)
      .setNotBefore(0);
    if (this.identity) {
      jwt.setSubject(this.identity);
    } else if (this.grants.video?.roomJoin) {
      throw Error('identity is required for join but not set');
    }
    return jwt.sign(secret);
  }
}

export class TokenVerifier {
  private apiKey: string;

  private apiSecret: string;

  constructor(apiKey: string, apiSecret: string) {
    this.apiKey = apiKey;
    this.apiSecret = apiSecret;
  }

  async verify(
    token: string,
    clockTolerance: string | number = defaultClockToleranceSeconds,
  ): Promise<ClaimGrants> {
    const secret = new TextEncoder().encode(this.apiSecret);
    const { payload } = await jose.jwtVerify(token, secret, {
      issuer: this.apiKey,
      clockTolerance,
    });
    if (!payload) {
      throw Error('invalid token');
    }

    return payload as ClaimGrants;
  }
}
