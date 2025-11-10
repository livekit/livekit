// SPDX-FileCopyrightText: 2024 LiveKit, Inc.
//
// SPDX-License-Identifier: Apache-2.0
import { TrackSource } from '@livekit/protocol';
import { describe, expect, it } from 'vitest';
import type { ClaimGrants, VideoGrant } from './grants.js';
import { claimsToJwtPayload } from './grants.js';

describe('ClaimGrants are parsed correctly', () => {
  it('parses TrackSource correctly to strings', () => {
    const grant: VideoGrant = {
      canPublishSources: [
        TrackSource.CAMERA,
        TrackSource.MICROPHONE,
        TrackSource.SCREEN_SHARE,
        TrackSource.SCREEN_SHARE_AUDIO,
      ],
    };

    const claim: ClaimGrants = { video: grant };

    const jwtPayload = claimsToJwtPayload(claim);
    expect(jwtPayload.video).toBeTypeOf('object');
    expect(jwtPayload.video?.canPublishSources).toEqual([
      'camera',
      'microphone',
      'screen_share',
      'screen_share_audio',
    ]);
  });
});
