import { describe, expect, it } from 'vitest';
import { ScreenSharePresets, VideoPreset, VideoPresets, VideoPresets43 } from '../track/options';
import {
  computeDefaultScreenShareSimulcastPresets,
  computeVideoEncodings,
  determineAppropriateEncoding,
  presets43,
  presets169,
  presetsForResolution,
  presetsScreenShare,
  sortPresets,
} from './publishUtils';

describe('presetsForResolution', () => {
  it('handles screenshare', () => {
    expect(presetsForResolution(true, 600, 300)).toEqual(presetsScreenShare);
  });

  it('handles landscape', () => {
    expect(presetsForResolution(false, 600, 300)).toEqual(presets169);
    expect(presetsForResolution(false, 500, 500)).toEqual(presets43);
  });

  it('handles portrait', () => {
    expect(presetsForResolution(false, 300, 600)).toEqual(presets169);
    expect(presetsForResolution(false, 500, 500)).toEqual(presets43);
  });
});

describe('determineAppropriateEncoding', () => {
  it('uses higher encoding', () => {
    expect(determineAppropriateEncoding(false, 600, 300)).toEqual(VideoPresets.h360.encoding);
  });

  it('handles portrait', () => {
    expect(determineAppropriateEncoding(false, 300, 600)).toEqual(VideoPresets.h360.encoding);
  });
});

describe('computeVideoEncodings', () => {
  it('handles non-simulcast', () => {
    const encodings = computeVideoEncodings(false, 640, 480, {
      simulcast: false,
    });
    expect(encodings).toEqual([{}]);
  });

  it('respects client defined bitrate', () => {
    const encodings = computeVideoEncodings(false, 640, 480, {
      simulcast: false,
      videoEncoding: {
        maxBitrate: 1024,
      },
    });
    expect(encodings).toHaveLength(1);
    expect(encodings![0].maxBitrate).toBe(1024);
  });

  it('returns three encodings for high-res simulcast', () => {
    const encodings = computeVideoEncodings(false, 960, 540, {
      simulcast: true,
    });
    expect(encodings).toHaveLength(3);

    // ensure they are what we expect
    expect(encodings![0].rid).toBe('q');
    expect(encodings![0].maxBitrate).toBe(VideoPresets.h180.encoding.maxBitrate);
    expect(encodings![0].scaleResolutionDownBy).toBe(3);
    expect(encodings![1].rid).toBe('h');
    expect(encodings![1].scaleResolutionDownBy).toBe(1.5);
    expect(encodings![2].rid).toBe('f');
  });

  it('handles portrait simulcast', () => {
    const encodings = computeVideoEncodings(false, 540, 960, {
      simulcast: true,
    });
    expect(encodings).toHaveLength(3);
    expect(encodings![0].scaleResolutionDownBy).toBe(3);
    expect(encodings![1].scaleResolutionDownBy).toBe(1.5);
    expect(encodings![2].maxBitrate).toBe(VideoPresets.h540.encoding.maxBitrate);
  });

  it('returns two encodings for lower-res simulcast', () => {
    const encodings = computeVideoEncodings(false, 640, 360, {
      simulcast: true,
    });
    expect(encodings).toHaveLength(2);

    // ensure they are what we expect
    expect(encodings![0].rid).toBe('q');
    expect(encodings![0].maxBitrate).toBe(VideoPresets.h180.encoding.maxBitrate);
    expect(encodings![1].rid).toBe('h');
    expect(encodings![1].maxBitrate).toBe(VideoPresets.h360.encoding.maxBitrate);
  });

  it('returns one encoding if an empty array is provided for custom screen share layers', () => {
    const encodings = computeVideoEncodings(true, 1920, 1080, {
      simulcast: true,
      screenShareSimulcastLayers: [],
    });
    expect(encodings).toHaveLength(1);

    // ensure they are what we expect
    expect(encodings![0].rid).toBe('q');
    expect(encodings![0].scaleResolutionDownBy).toBe(1);
  });

  it('respects provided min resolution', () => {
    const encodings = computeVideoEncodings(false, 100, 120, {
      simulcast: true,
    });
    expect(encodings).toHaveLength(1);
    expect(encodings![0].rid).toBe('q');
    expect(encodings![0].maxBitrate).toBe(VideoPresets43.h120.encoding.maxBitrate);
    expect(encodings![0].scaleResolutionDownBy).toBe(1);
  });

  //   it('respects default backup codec encoding', () => {
  //     const vp8Encodings = computeTrackBackupEncodings(false, 100, 120, { simulcast: true });
  //     const h264Encodings = computeVideoEncodings(false, 100, 120, {
  //       simulcast: true,
  //       videoCodec: 'h264',
  //     });
  //     const av1Encodings = computeVideoEncodings(false, 100, 120, {
  //       simulcast: true,
  //       videoCodec: 'av1',
  //     });
  //     expect(h264Encodings).toHaveLength(1);
  //     expect(h264Encodings![0].rid).toBe('q');
  //     expect(h264Encodings![0].maxBitrate).toBe(vp8Encodings[0].maxBitrate! * 1.1);
  //     expect(av1Encodings![0].maxBitrate).toBe(vp8Encodings[0].maxBitrate! * 0.7);
  //     expect(h264Encodings![0].scaleResolutionDownBy).toBe(1);
  //   });

  //   it('respects custom backup codec encoding', () => {
  //     const encodings = computeVideoEncodings(false, 100, 120, {
  //       simulcast: true,
  //       videoCodec: 'h264',
  //       backupCodec: {
  //         vp8: { maxBitrate: 1_000 },
  //         h264: { maxBitrate: 2_000 },
  //       },
  //     });
  //     expect(encodings).toHaveLength(1);
  //     expect(encodings![0].rid).toBe('q');
  //     expect(encodings![0].maxBitrate).toBe(2_000);
  //     expect(encodings![0].scaleResolutionDownBy).toBe(1);
  //   });
});

describe('customSimulcastLayers', () => {
  it('sorts presets from lowest to highest', () => {
    const sortedPresets = sortPresets([
      VideoPresets.h1440,
      VideoPresets.h360,
      VideoPresets.h1080,
      VideoPresets.h90,
    ]) as Array<VideoPreset>;
    expect(sortPresets).not.toBeUndefined();
    expect(sortedPresets[0]).toBe(VideoPresets.h90);
    expect(sortedPresets[1]).toBe(VideoPresets.h360);
    expect(sortedPresets[2]).toBe(VideoPresets.h1080);
    expect(sortedPresets[3]).toBe(VideoPresets.h1440);
  });
  it('sorts presets from lowest to highest, even when dimensions are the same', () => {
    const sortedPresets = sortPresets([
      new VideoPreset(1920, 1080, 3_000_000, 20),
      new VideoPreset(1920, 1080, 2_000_000, 15),
      new VideoPreset(1920, 1080, 3_000_000, 15),
    ]) as Array<VideoPreset>;
    expect(sortPresets).not.toBeUndefined();
    expect(sortedPresets[0].encoding.maxBitrate).toBe(2_000_000);
    expect(sortedPresets[1].encoding.maxFramerate).toBe(15);
    expect(sortedPresets[2].encoding.maxFramerate).toBe(20);
  });
});

describe('screenShareSimulcastDefaults', () => {
  it('computes appropriate bitrate from original preset', () => {
    const defaultSimulcastLayers = computeDefaultScreenShareSimulcastPresets(
      ScreenSharePresets.h720fps15,
    );
    expect(defaultSimulcastLayers[0].width).toBe(640);
    expect(defaultSimulcastLayers[0].height).toBe(360);
    expect(defaultSimulcastLayers[0].encoding.maxFramerate).toBe(15);
    expect(defaultSimulcastLayers[0].encoding.maxBitrate).toBe(375000);
  });
});
