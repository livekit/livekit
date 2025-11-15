import { LivekitError } from '../room/errors';

export enum CryptorErrorReason {
  InvalidKey = 0,
  MissingKey = 1,
  InternalError = 2,
}

export class CryptorError extends LivekitError {
  reason: CryptorErrorReason;

  participantIdentity?: string;

  constructor(
    message?: string,
    reason: CryptorErrorReason = CryptorErrorReason.InternalError,
    participantIdentity?: string,
  ) {
    super(40, message);
    this.reason = reason;
    this.participantIdentity = participantIdentity;
  }
}
