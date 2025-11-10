// SPDX-FileCopyrightText: 2024 LiveKit, Inc.
//
// SPDX-License-Identifier: Apache-2.0
import type { BinaryReadOptions, JsonReadOptions, JsonValue } from '@bufbuild/protobuf';
import { WebhookEvent as ProtoWebhookEvent } from '@livekit/protocol';
import { TokenVerifier } from './AccessToken.js';
import { digest } from './crypto/digest.js';

export const authorizeHeader = 'Authorize';

export class WebhookEvent extends ProtoWebhookEvent {
  event: WebhookEventNames = '';

  static fromBinary(bytes: Uint8Array, options?: Partial<BinaryReadOptions>): WebhookEvent {
    return new WebhookEvent().fromBinary(bytes, options);
  }

  static fromJson(jsonValue: JsonValue, options?: Partial<JsonReadOptions>): WebhookEvent {
    return new WebhookEvent().fromJson(jsonValue, options);
  }

  static fromJsonString(jsonString: string, options?: Partial<JsonReadOptions>): WebhookEvent {
    return new WebhookEvent().fromJsonString(jsonString, options);
  }
}

export type WebhookEventNames =
  | 'room_started'
  | 'room_finished'
  | 'participant_joined'
  | 'participant_left'
  | 'participant_connection_aborted'
  | 'track_published'
  | 'track_unpublished'
  | 'egress_started'
  | 'egress_updated'
  | 'egress_ended'
  | 'ingress_started'
  | 'ingress_ended'
  /**
   * @internal
   * @remarks only used as a default value, not a valid webhook event
   */
  | '';

export class WebhookReceiver {
  private verifier: TokenVerifier;

  constructor(apiKey: string, apiSecret: string) {
    this.verifier = new TokenVerifier(apiKey, apiSecret);
  }

  /**
   * @param body - string of the posted body
   * @param authHeader - `Authorization` header from the request
   * @param skipAuth - true to skip auth validation
   * @param clockTolerance - How much tolerance to allow for checks against the auth header to be skewed from the claims
   * @returns The processed webhook event
   */
  async receive(
    body: string,
    authHeader?: string,
    skipAuth: boolean = false,
    clockTolerance?: string | number,
  ): Promise<WebhookEvent> {
    // verify token
    if (!skipAuth) {
      if (!authHeader) {
        throw new Error('authorization header is empty');
      }
      const claims = await this.verifier.verify(authHeader, clockTolerance);
      // confirm sha
      const hash = await digest(body);
      const hashDecoded = btoa(
        Array.from(new Uint8Array(hash))
          .map((v) => String.fromCharCode(v))
          .join(''),
      );

      if (claims.sha256 !== hashDecoded) {
        throw new Error('sha256 checksum of body does not match');
      }
    }

    return WebhookEvent.fromJson(JSON.parse(body), { ignoreUnknownFields: true });
  }
}
