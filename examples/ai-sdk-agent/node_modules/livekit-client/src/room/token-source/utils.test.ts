import { TokenSourceResponse } from '@livekit/protocol';
import { describe, expect, it } from 'vitest';
import { decodeTokenPayload, isResponseTokenValid } from './utils';

// Test JWTs created for test purposes only.
// None of these actually auth against anything.
const TOKENS = {
  // Nbf date set at 1234567890 seconds (Fri Feb 13 2009 23:31:30 GMT+0000)
  // Exp date set at 9876543210 seconds (Fri Dec 22 2282 20:13:30 GMT+0000)
  // A dummy roomConfig value is also set, with room_config.name = "test room name", and room_config.agents = [{"agentName": "test agent name","metadata":"test agent metadata"}]
  VALID:
    'eyJhbGciOiJFUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwiZXhwIjo5ODc2NTQzMjEwLCJuYmYiOjEyMzQ1Njc4OTAsImlhdCI6MTIzNDU2Nzg5MCwicm9vbUNvbmZpZyI6eyJuYW1lIjoidGVzdCByb29tIG5hbWUiLCJlbXB0eVRpbWVvdXQiOjAsImRlcGFydHVyZVRpbWVvdXQiOjAsIm1heFBhcnRpY2lwYW50cyI6MCwibWluUGxheW91dERlbGF5IjowLCJtYXhQbGF5b3V0RGVsYXkiOjAsInN5bmNTdHJlYW1zIjpmYWxzZSwiYWdlbnRzIjpbeyJhZ2VudE5hbWUiOiJ0ZXN0IGFnZW50IG5hbWUiLCJtZXRhZGF0YSI6InRlc3QgYWdlbnQgbWV0YWRhdGEifV0sIm1ldGFkYXRhIjoiIn19.EDetpHG8cSubaApzgWJaQrpCiSy9KDBlfCfVdIydbQ-_CHiNnXOK_f_mCJbTf9A-duT1jmvPOkLrkkWFT60XPQ',

  // Nbf date set at 9876543210 seconds (Fri Dec 22 2282 20:13:30 GMT+0000)
  // Exp date set at 9876543211 seconds (Fri Dec 22 2282 20:13:31 GMT+0000)
  NBF_IN_FUTURE:
    'eyJhbGciOiJFUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwiZXhwIjo5ODc2NTQzMjExLCJuYmYiOjk4NzY1NDMyMTAsImlhdCI6MTIzNDU2Nzg5MH0.DcMmdKrD76eJg7IUBZqoTRDvBaXtCcwtuE5h7IwVXhG_6nvgxN_ix30_AmLgnYhvhkN-x9dTRPoHg-CME72AbQ',

  // Nbf date set at 1234567890 seconds (Fri Feb 13 2009 23:31:30 GMT+0000)
  // Exp date set at 1234567891 seconds (Fri Feb 13 2009 23:31:31 GMT+0000)
  EXP_IN_PAST:
    'eyJhbGciOiJFUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwiZXhwIjoxMjM0NTY3ODkxLCJuYmYiOjEyMzQ1Njc4OTAsImlhdCI6MTIzNDU2Nzg5MH0.OYP1NITayotBYt0mioInLJmaIM0bHyyR-yG6iwKyQDzhoGha15qbsc7dOJlzz4za1iW5EzCgjc2_xGxqaSu5XA',
};

describe('isResponseTokenValid', () => {
  it('should find a valid jwt not expired', () => {
    const isValid = isResponseTokenValid(
      TokenSourceResponse.fromJson({
        serverUrl: 'ws://localhost:7800',
        participantToken: TOKENS.VALID,
      }),
    );
    expect(isValid).toBe(true);
  });
  it('should find a long ago expired jwt as expired', () => {
    const isValid = isResponseTokenValid(
      TokenSourceResponse.fromJson({
        serverUrl: 'ws://localhost:7800',
        participantToken: TOKENS.EXP_IN_PAST,
      }),
    );
    expect(isValid).toBe(false);
  });
  it('should find a jwt that has not become active yet as expired', () => {
    const isValid = isResponseTokenValid(
      TokenSourceResponse.fromJson({
        serverUrl: 'ws://localhost:7800',
        participantToken: TOKENS.NBF_IN_FUTURE,
      }),
    );
    expect(isValid).toBe(false);
  });
});

describe('decodeTokenPayload', () => {
  it('should extract roomconfig metadata from a token', () => {
    const payload = decodeTokenPayload(TOKENS.VALID);
    expect(payload.roomConfig?.name).toBe('test room name');
    expect(payload.roomConfig?.agents).toHaveLength(1);
    expect(payload.roomConfig?.agents![0].agentName).toBe('test agent name');
    expect(payload.roomConfig?.agents![0].metadata).toBe('test agent metadata');
  });
});
