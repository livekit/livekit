import { RoomConfiguration, TokenSourceRequest, TokenSourceResponse } from '@livekit/protocol';
import type { JWTPayload } from 'jose';
import type { ValueToSnakeCase } from '../../utils/camelToSnakeCase';
// The below imports are being linked in tsdoc comments, so they have to be imported even if they
// aren't being used.
// eslint-disable-next-line @typescript-eslint/no-unused-vars
import type { TokenSourceCustom, TokenSourceEndpoint, TokenSourceLiteral } from './TokenSource';

export type TokenSourceRequestObject = Required<
  NonNullable<ConstructorParameters<typeof TokenSourceRequest>[0]>
>;
export type TokenSourceResponseObject = Required<
  NonNullable<ConstructorParameters<typeof TokenSourceResponse>[0]>
>;

/** The `TokenSource` request object sent to the server as part of fetching a configurable
 * `TokenSource` like {@link TokenSourceEndpoint}.
 *
 * Use this as a type for your request body if implementing a server endpoint in node.js.
 */
export type TokenSourceRequestPayload = ValueToSnakeCase<TokenSourceRequestObject>;

/** The `TokenSource` response object sent from the server as part of fetching a configurable
 * `TokenSource` like {@link TokenSourceEndpoint}.
 *
 * Use this as a type for your response body if implementing a server endpoint in node.js.
 */
export type TokenSourceResponsePayload = ValueToSnakeCase<TokenSourceResponseObject>;

/** The payload of a LiveKit JWT token. */
export type TokenPayload = JWTPayload & {
  name?: string;
  metadata?: string;
  attributes?: Record<string, string>;
  video?: {
    room?: string;
    roomJoin?: boolean;
    canPublish?: boolean;
    canPublishData?: boolean;
    canSubscribe?: boolean;
  };
  roomConfig?: RoomConfigurationObject;
};
export type RoomConfigurationObject = NonNullable<
  ConstructorParameters<typeof RoomConfiguration>[0]
>;

/** A Fixed TokenSource is a token source that takes no parameters and returns a completely
 * independently derived value on each fetch() call.
 *
 * The most common downstream implementer is {@link TokenSourceLiteral}.
 */
export abstract class TokenSourceFixed {
  abstract fetch(): Promise<TokenSourceResponseObject>;
}

export type TokenSourceFetchOptions = {
  roomName?: string;
  participantName?: string;
  participantIdentity?: string;
  participantMetadata?: string;
  participantAttributes?: { [key: string]: string };

  agentName?: string;
  agentMetadata?: string;
};

/** A Configurable TokenSource is a token source that takes a
 * {@link TokenSourceFetchOptions} object as input and returns a deterministic
 * {@link TokenSourceResponseObject} output based on the options specified.
 *
 * For example, if options.participantName is set, it should be expected that
 * all tokens that are generated will have participant name field set to the
 * provided value.
 *
 * A few common downstream implementers are {@link TokenSourceEndpoint}
 * and {@link TokenSourceCustom}.
 */
export abstract class TokenSourceConfigurable {
  abstract fetch(options: TokenSourceFetchOptions): Promise<TokenSourceResponseObject>;
}

/** A TokenSource is a mechanism for fetching credentials required to connect to a LiveKit Room. */
export type TokenSourceBase = TokenSourceFixed | TokenSourceConfigurable;
