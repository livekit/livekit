import log from '../../logger';
import { isLocalTrack } from '../utils';
import LocalTrack from './LocalTrack';
import type { VideoCaptureOptions } from './options';

type FacingMode = NonNullable<VideoCaptureOptions['facingMode']>;
type FacingModeFromLocalTrackOptions = {
  /**
   * If no facing mode can be determined, this value will be used.
   * @defaultValue 'user'
   */
  defaultFacingMode?: FacingMode;
};
type FacingModeFromLocalTrackReturnValue = {
  /**
   * The (probable) facingMode of the track.
   * @see {@link https://developer.mozilla.org/en-US/docs/Web/API/MediaTrackConstraints/facingMode | MDN docs on facingMode}
   */
  facingMode: FacingMode;
  /**
   * The confidence that the returned facingMode is correct.
   */
  confidence: 'high' | 'medium' | 'low';
};

/**
 * Try to analyze the local track to determine the facing mode of a track.
 *
 * @remarks
 * There is no property supported by all browsers to detect whether a video track originated from a user- or environment-facing camera device.
 * For this reason, we use the `facingMode` property when available, but will fall back on a string-based analysis of the device label to determine the facing mode.
 * If both methods fail, the default facing mode will be used.
 *
 * @see {@link https://developer.mozilla.org/en-US/docs/Web/API/MediaTrackConstraints/facingMode | MDN docs on facingMode}
 * @experimental
 */
export function facingModeFromLocalTrack(
  localTrack: LocalTrack | MediaStreamTrack,
  options: FacingModeFromLocalTrackOptions = {},
): FacingModeFromLocalTrackReturnValue {
  const track = isLocalTrack(localTrack) ? localTrack.mediaStreamTrack : localTrack;
  const trackSettings = track.getSettings();
  let result: FacingModeFromLocalTrackReturnValue = {
    facingMode: options.defaultFacingMode ?? 'user',
    confidence: 'low',
  };

  // 1. Try to get facingMode from track settings.
  if ('facingMode' in trackSettings) {
    const rawFacingMode = trackSettings.facingMode;
    log.trace('rawFacingMode', { rawFacingMode });
    if (rawFacingMode && typeof rawFacingMode === 'string' && isFacingModeValue(rawFacingMode)) {
      result = { facingMode: rawFacingMode, confidence: 'high' };
    }
  }

  // 2. If we don't have a high confidence we try to get the facing mode from the device label.
  if (['low', 'medium'].includes(result.confidence)) {
    log.trace(`Try to get facing mode from device label: (${track.label})`);
    const labelAnalysisResult = facingModeFromDeviceLabel(track.label);
    if (labelAnalysisResult !== undefined) {
      result = labelAnalysisResult;
    }
  }

  return result;
}

const knownDeviceLabels = new Map<string, FacingModeFromLocalTrackReturnValue>([
  ['obs virtual camera', { facingMode: 'environment', confidence: 'medium' }],
]);
const knownDeviceLabelSections = new Map<string, FacingModeFromLocalTrackReturnValue>([
  ['iphone', { facingMode: 'environment', confidence: 'medium' }],
  ['ipad', { facingMode: 'environment', confidence: 'medium' }],
]);
/**
 * Attempt to analyze the device label to determine the facing mode.
 *
 * @experimental
 */
export function facingModeFromDeviceLabel(
  deviceLabel: string,
): FacingModeFromLocalTrackReturnValue | undefined {
  const label = deviceLabel.trim().toLowerCase();
  // Empty string is a valid device label but we can't infer anything from it.
  if (label === '') {
    return undefined;
  }

  // Can we match against widely known device labels.
  if (knownDeviceLabels.has(label)) {
    return knownDeviceLabels.get(label);
  }

  // Can we match against sections of the device label.
  return Array.from(knownDeviceLabelSections.entries()).find(([section]) =>
    label.includes(section),
  )?.[1];
}

function isFacingModeValue(item: string): item is FacingMode {
  const allowedValues: FacingMode[] = ['user', 'environment', 'left', 'right'];
  return item === undefined || allowedValues.includes(item as FacingMode);
}
