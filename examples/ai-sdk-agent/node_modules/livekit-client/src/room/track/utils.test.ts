import { describe, expect, it } from 'vitest';
import { audioDefaults, videoDefaults } from '../defaults';
import { type AudioCaptureOptions, VideoPresets } from './options';
import { constraintsForOptions, diffAttributes, mergeDefaultOptions } from './utils';

describe('mergeDefaultOptions', () => {
  it('does not enable undefined options', () => {
    const opts = mergeDefaultOptions(undefined, audioDefaults, videoDefaults);
    expect(opts.audio).toEqual(undefined);
    expect(opts.video).toEqual(undefined);
  });

  it('does not enable explicitly disabled', () => {
    const opts = mergeDefaultOptions({
      video: false,
    });
    expect(opts.audio).toEqual(undefined);
    expect(opts.video).toEqual(false);
  });

  it('accepts true for options', () => {
    const opts = mergeDefaultOptions(
      {
        audio: true,
      },
      audioDefaults,
      videoDefaults,
    );
    expect(opts.audio).toEqual(audioDefaults);
    expect(opts.video).toEqual(undefined);
  });

  it('enables overriding specific fields', () => {
    const opts = mergeDefaultOptions(
      {
        audio: { channelCount: 1 },
      },
      audioDefaults,
      videoDefaults,
    );
    const audioOpts = opts.audio as AudioCaptureOptions;
    expect(audioOpts.channelCount).toEqual(1);
    expect(audioOpts.autoGainControl).toEqual(true);
  });

  it('does not override explicit false', () => {
    const opts = mergeDefaultOptions(
      {
        audio: { autoGainControl: false },
      },
      audioDefaults,
      videoDefaults,
    );
    const audioOpts = opts.audio as AudioCaptureOptions;
    expect(audioOpts.autoGainControl).toEqual(false);
  });
});

describe('constraintsForOptions', () => {
  it('correctly enables audio bool', () => {
    const constraints = constraintsForOptions({
      audio: true,
    });
    expect(constraints.audio).toEqual({ deviceId: audioDefaults.deviceId });
    expect(constraints.video).toEqual(false);
  });

  it('converts audio options correctly', () => {
    const constraints = constraintsForOptions({
      audio: {
        noiseSuppression: true,
        echoCancellation: false,
      },
    });
    const audioOpts = constraints.audio as MediaTrackConstraints;
    expect(Object.keys(audioOpts)).toEqual(['noiseSuppression', 'echoCancellation', 'deviceId']);
    expect(audioOpts.noiseSuppression).toEqual(true);
    expect(audioOpts.echoCancellation).toEqual(false);
  });

  it('converts video options correctly', () => {
    const constraints = constraintsForOptions({
      video: {
        resolution: VideoPresets.h720.resolution,
        facingMode: 'user',
        deviceId: 'video123',
      },
    });
    const videoOpts = constraints.video as MediaTrackConstraints;
    expect(Object.keys(videoOpts)).toEqual([
      'width',
      'height',
      'frameRate',
      'aspectRatio',
      'facingMode',
      'deviceId',
    ]);
    expect(videoOpts.width).toEqual(VideoPresets.h720.resolution.width);
    expect(videoOpts.height).toEqual(VideoPresets.h720.resolution.height);
    expect(videoOpts.frameRate).toEqual(VideoPresets.h720.resolution.frameRate);
    expect(videoOpts.aspectRatio).toEqual(VideoPresets.h720.resolution.aspectRatio);
  });
});

describe('diffAttributes', () => {
  it('detects changed values', () => {
    const oldValues: Record<string, string> = { a: 'value', b: 'initial', c: 'value' };
    const newValues: Record<string, string> = { a: 'value', b: 'updated', c: 'value' };

    const diff = diffAttributes(oldValues, newValues);
    expect(Object.keys(diff).length).toBe(1);
    expect(diff.b).toBe('updated');
  });
  it('detects new values', () => {
    const newValues: Record<string, string> = { a: 'value', b: 'value', c: 'value' };
    const oldValues: Record<string, string> = { a: 'value', b: 'value' };

    const diff = diffAttributes(oldValues, newValues);
    expect(Object.keys(diff).length).toBe(1);
    expect(diff.c).toBe('value');
  });
  it('detects deleted values as empty strings', () => {
    const newValues: Record<string, string> = { a: 'value', b: 'value' };
    const oldValues: Record<string, string> = { a: 'value', b: 'value', c: 'value' };

    const diff = diffAttributes(oldValues, newValues);
    expect(Object.keys(diff).length).toBe(1);
    expect(diff.c).toBe('');
  });
  it('compares with undefined values', () => {
    const newValues: Record<string, string> = { a: 'value', b: 'value' };

    const diff = diffAttributes(undefined, newValues);
    expect(Object.keys(diff).length).toBe(2);
    expect(diff.a).toBe('value');
  });
});
