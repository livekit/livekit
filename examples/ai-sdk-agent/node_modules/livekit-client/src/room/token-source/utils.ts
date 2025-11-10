import { RoomConfiguration, type TokenSourceResponse } from '@livekit/protocol';
import { decodeJwt } from 'jose';
import type { RoomConfigurationObject, TokenPayload } from './types';

const ONE_SECOND_IN_MILLISECONDS = 1000;
const ONE_MINUTE_IN_MILLISECONDS = 60 * ONE_SECOND_IN_MILLISECONDS;

export function isResponseTokenValid(response: TokenSourceResponse) {
  const jwtPayload = decodeTokenPayload(response.participantToken);
  if (!jwtPayload?.nbf || !jwtPayload?.exp) {
    return true;
  }

  const now = new Date();

  const nbfInMilliseconds = jwtPayload.nbf * ONE_SECOND_IN_MILLISECONDS;
  const nbfDate = new Date(nbfInMilliseconds);

  const expInMilliseconds = jwtPayload.exp * ONE_SECOND_IN_MILLISECONDS;
  const expDate = new Date(expInMilliseconds - ONE_MINUTE_IN_MILLISECONDS);

  return nbfDate <= now && expDate > now;
}

/** Given a LiveKit generated participant token, decodes and returns the associated {@link TokenPayload} data. */
export function decodeTokenPayload(token: string) {
  const payload = decodeJwt<Omit<TokenPayload, 'roomConfig'>>(token);

  const { roomConfig, ...rest } = payload;

  const mappedPayload: TokenPayload = {
    ...rest,
    roomConfig: payload.roomConfig
      ? (RoomConfiguration.fromJson(
          payload.roomConfig as Record<string, any>,
        ) as RoomConfigurationObject)
      : undefined,
  };

  return mappedPayload;
}
